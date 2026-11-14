package auth

import (
	"crypto/subtle"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/store"
)

// npm tokens are "clwn_<id>_<secret>". The prefix differs from the admin and project tokens'
// "clw_", so Check refuses an npm token before any lookup, and CheckNPM refuses the others:
// neither kind works where the other belongs.
const npmPrefix = "clwn_"

const (
	// NPMDefaultTTL is how long an npm token lives unless asked otherwise.
	NPMDefaultTTL = 365 * 24 * time.Hour
	// NPMRotateGrace is how long a rotated-away token keeps working.
	NPMRotateGrace = 24 * time.Hour
)

// Scope kinds.
const (
	ScopeNPMRead    = "npm:read"
	ScopeNPMPublish = "npm:publish"
	ScopePlugins    = "plugins:read"
)

var (
	// PackageNameRe is a scoped npm package name. npm allows more (upper case in old names,
	// unscoped names); this registry serves only scoped, lower-case, URL-safe names.
	PackageNameRe = regexp.MustCompile(`^@[a-z0-9][a-z0-9._~-]*/[a-z0-9][a-z0-9._~-]*$`)
	patternRe     = regexp.MustCompile(`^@[a-z0-9][a-z0-9._~-]*/(\*|[a-z0-9][a-z0-9._~-]*)$`)
)

// ValidPackageName reports whether name is a scoped package name this registry accepts.
func ValidPackageName(name string) bool {
	return len(name) <= 214 && PackageNameRe.MatchString(name)
}

// ValidScope reports whether s is a scope an npm token may carry.
func ValidScope(s string) error {
	if s == ScopePlugins {
		return nil
	}
	for _, k := range []string{ScopeNPMRead, ScopeNPMPublish} {
		if p, ok := strings.CutPrefix(s, k+":"); ok {
			if len(p) > 214 || !patternRe.MatchString(p) {
				return fmt.Errorf("scope %q: the pattern must be @scope/name or @scope/*", s)
			}
			return nil
		}
	}
	return fmt.Errorf("scope %q: want npm:read:<pattern>, npm:publish:<pattern> or plugins:read", s)
}

func patternMatches(pattern, name string) bool {
	if scope, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(name, scope+"/")
	}
	return pattern == name
}

// NPMRights are what an npm token may do.
type NPMRights struct{ Scopes []string }

func (r NPMRights) has(kind, name string) bool {
	for _, s := range r.Scopes {
		if p, ok := strings.CutPrefix(s, kind+":"); ok && patternMatches(p, name) {
			return true
		}
	}
	return false
}

// CanRead reports whether the token may read the package. Publishing implies reading: npm
// fetches the document before it publishes.
func (r NPMRights) CanRead(name string) bool {
	return ValidPackageName(name) && (r.has(ScopeNPMRead, name) || r.has(ScopeNPMPublish, name))
}

// CanPublish reports whether the token may publish the package and move its dist-tags.
func (r NPMRights) CanPublish(name string) bool {
	return ValidPackageName(name) && r.has(ScopeNPMPublish, name)
}

// Plugins reports whether the token may read the plugin marketplace.
func (r NPMRights) Plugins() bool {
	for _, s := range r.Scopes {
		if s == ScopePlugins {
			return true
		}
	}
	return false
}

// NewNPM creates an npm token; the secret is shown once. idTaken guards against an id that is
// already used by a token of either kind.
func NewNPM(name string, scopes []string, created time.Time, ttl time.Duration, idTaken func(string) bool) (string, store.NPMToken) {
	if ttl <= 0 {
		ttl = NPMDefaultTTL
	}
	id := randHex(6)
	for idTaken != nil && idTaken(id) {
		id = randHex(6)
	}
	secret := npmPrefix + id + "_" + randHex(24)
	return secret, store.NPMToken{
		ID:      id,
		Hash:    hash(secret),
		Name:    name,
		Scopes:  append([]string(nil), scopes...),
		Created: created.UTC(),
		Expires: created.UTC().Add(ttl),
	}
}

type NPMTokens interface {
	NPMToken(id string) (store.NPMToken, error)
}

// CheckNPM validates an npm token: known, matching hash and not expired. A rotated-away token
// carries the end of its grace period in Expires.
func CheckNPM(ts NPMTokens, secret string, now time.Time) (store.NPMToken, error) {
	rest, ok := strings.CutPrefix(secret, npmPrefix)
	if !ok {
		return store.NPMToken{}, ErrUnauthorized
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok || id == "" {
		return store.NPMToken{}, ErrUnauthorized
	}
	t, err := ts.NPMToken(id)
	if err != nil {
		return store.NPMToken{}, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash(secret))) != 1 {
		return store.NPMToken{}, ErrUnauthorized
	}
	if !now.Before(t.Expires) {
		return store.NPMToken{}, ErrUnauthorized
	}
	return t, nil
}

// IsNPMToken reports whether s looks like an npm token (for messages, never for access).
func IsNPMToken(s string) bool { return strings.HasPrefix(s, npmPrefix) }
