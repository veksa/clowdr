// Package auth issues and checks tokens. A token is "clw_<id>_<secret>"; only the sha256 of
// the whole token is stored, the id is used for lookup.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/veksa/clowdr/internal/store"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

const prefix = "clw_"

// Principal is who made a request.
type Principal struct {
	TokenID string
	Admin   bool
	Project string // empty for admin
	Env     string
}

// CanProject reports whether p may act on project.
func (p Principal) CanProject(project string) bool {
	return p.Admin || (p.Project != "" && p.Project == project)
}

// Name is used as the release author fallback and in logs.
func (p Principal) Name() string {
	if p.Admin {
		return "admin"
	}
	return p.Project
}

// New creates a token and returns the secret (shown once) and the record to store.
func New(project, env, name string, admin bool) (string, store.Token) {
	id := randHex(6)
	secret := prefix + id + "_" + randHex(24)
	return secret, store.Token{
		ID:      id,
		Hash:    hash(secret),
		Project: project,
		Env:     env,
		Admin:   admin,
		Name:    name,
	}
}

type Tokens interface {
	Token(id string) (store.Token, error)
}

// Check validates a secret against the store.
func Check(ts Tokens, secret string) (Principal, error) {
	rest, ok := strings.CutPrefix(secret, prefix)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok || id == "" {
		return Principal{}, ErrUnauthorized
	}
	t, err := ts.Token(id)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash(secret))) != 1 {
		return Principal{}, ErrUnauthorized
	}
	return Principal{TokenID: t.ID, Admin: t.Admin, Project: t.Project, Env: t.Env}, nil
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// DomainAllowed reports whether domain matches one of the project's allowed patterns.
// "x.com" matches only itself; "*.x.com" matches exactly one label below x.com, and also
// the pattern "*.x.com" itself (a wildcard server_name).
func DomainAllowed(patterns []string, domain string) bool {
	domain = strings.ToLower(domain)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if p == domain {
			return true
		}
		if base, ok := strings.CutPrefix(p, "*."); ok {
			label, rest, ok := strings.Cut(domain, ".")
			if ok && rest == base && label != "" && label != "*" {
				return true
			}
		}
	}
	return false
}
