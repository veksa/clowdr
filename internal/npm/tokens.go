package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/store"
)

// Errors of token management, mapped to statuses by the API.
var (
	ErrTokenExists  = errors.New("a token with this name exists")
	ErrTokenRotated = errors.New("this token was rotated already")
)

var tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// Actor is who manages a token, for the audit log.
type Actor struct {
	By   string // "admin:<token id>" or "self"
	Addr string
}

// CreateToken issues an npm token. Names are unique: rotate keeps a name, create never reuses
// one that is still on record (revoke it first).
func (h *Handler) CreateToken(name string, scopes []string, ttl time.Duration, a Actor) (string, store.NPMToken, error) {
	if !tokenNameRe.MatchString(name) {
		return "", store.NPMToken{}, errors.New("token name: letters, digits, '.', '_', '@' and '-', up to 64")
	}
	if len(scopes) == 0 {
		return "", store.NPMToken{}, errors.New("a token needs at least one scope")
	}
	seen := map[string]bool{}
	var clean []string
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if err := auth.ValidScope(s); err != nil {
			return "", store.NPMToken{}, err
		}
		if !seen[s] {
			seen[s] = true
			clean = append(clean, s)
		}
	}
	if ttl < 0 {
		return "", store.NPMToken{}, errors.New("expiry must be in the future")
	}
	secret, t := auth.NewNPM(name, clean, h.now(), ttl, h.Store.TokenIDTaken)
	err := h.Store.UpdateNPMTokens(func(ts []store.NPMToken) ([]store.NPMToken, []string, error) {
		for _, o := range ts {
			if o.Name == name {
				return nil, nil, ErrTokenExists
			}
		}
		return []store.NPMToken{t}, nil, nil
	})
	if err != nil {
		return "", store.NPMToken{}, err
	}
	h.record(store.NPMAudit{Time: h.now(), Action: "token.create", Token: name, TokenID: t.ID, Addr: a.Addr,
		Detail: fmt.Sprintf("by %s, scopes %s, expires %s", a.By, strings.Join(clean, " "), t.Expires.Format("2006-01-02"))})
	t.Hash = ""
	return secret, t, nil
}

// Tokens lists npm tokens without their hashes.
func (h *Handler) Tokens() ([]store.NPMToken, error) {
	ts, err := h.Store.NPMTokens()
	for i := range ts {
		ts[i].Hash = ""
	}
	if ts == nil {
		ts = []store.NPMToken{}
	}
	return ts, err
}

// RevokeToken deletes every token with the name, including one still in a rotation's grace
// period. It takes effect on the next request.
func (h *Handler) RevokeToken(name string, a Actor) error {
	var ids []string
	err := h.Store.UpdateNPMTokens(func(ts []store.NPMToken) ([]store.NPMToken, []string, error) {
		for _, t := range ts {
			if t.Name == name {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) == 0 {
			return nil, nil, store.ErrNotFound
		}
		return nil, ids, nil
	})
	if err != nil {
		return err
	}
	h.record(store.NPMAudit{Time: h.now(), Action: "token.revoke", Token: name, Addr: a.Addr,
		Detail: "by " + a.By + ", ids " + strings.Join(ids, " ")})
	return nil
}

// RotateToken replaces the current token of the name (see rotate).
func (h *Handler) RotateToken(name string, immediate bool, a Actor) (string, store.NPMToken, error) {
	return h.rotate(func(t store.NPMToken) bool { return t.Name == name && t.ReplacedBy == "" }, immediate, a)
}

// rotate issues a new token with the old one's name, scopes and lifetime. The old token keeps
// working for auth.NPMRotateGrace (its Expires is cut to then, never extended), or stops at
// once when immediate is set. A token that was rotated already cannot be rotated again: the
// holder of the old secret must not be able to mint a second successor.
func (h *Handler) rotate(match func(store.NPMToken) bool, immediate bool, a Actor) (string, store.NPMToken, error) {
	now := h.now()
	var secret string
	var nt, old store.NPMToken
	err := h.Store.UpdateNPMTokens(func(ts []store.NPMToken) ([]store.NPMToken, []string, error) {
		found := false
		for _, t := range ts {
			if match(t) && now.Before(t.Expires) {
				old, found = t, true
				break
			}
		}
		if !found {
			return nil, nil, store.ErrNotFound
		}
		if old.ReplacedBy != "" {
			return nil, nil, ErrTokenRotated
		}
		secret, nt = auth.NewNPM(old.Name, old.Scopes, now, old.Expires.Sub(old.Created), h.Store.TokenIDTaken)
		nt.RotatedFrom = old.ID
		if immediate {
			return []store.NPMToken{nt}, []string{old.ID}, nil
		}
		old.ReplacedBy = nt.ID
		if end := now.Add(auth.NPMRotateGrace); end.Before(old.Expires) {
			old.Expires = end
		}
		return []store.NPMToken{nt, old}, nil, nil
	})
	if err != nil {
		return "", store.NPMToken{}, err
	}
	detail := "by " + a.By + ", old " + old.ID + " works until " + old.Expires.Format(time.RFC3339)
	if immediate {
		detail = "by " + a.By + ", old " + old.ID + " revoked at once"
	}
	h.record(store.NPMAudit{Time: now, Action: "token.rotate", Token: nt.Name, TokenID: nt.ID, Addr: a.Addr, Detail: detail})
	nt.Hash = ""
	return secret, nt, nil
}

// RotateRequest is the optional body of a rotation.
type RotateRequest struct {
	// Now revokes the old token at once instead of after the grace period.
	Now bool `json:"now,omitempty"`
}

// TokenResponse carries a new npm token; the secret is shown once.
type TokenResponse struct {
	Token string         `json:"token"`
	Info  store.NPMToken `json:"info"`
}

// rotateSelf lets an npm token rotate itself, without an admin: a developer's machine holds
// only its npm token.
func (c *call) rotateSelf() {
	var req RotateRequest
	if c.r.ContentLength != 0 {
		dec := json.NewDecoder(io.LimitReader(c.r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(c.w, 400, "the body must be {\"now\": true|false}")
			return
		}
	}
	id := c.tok.ID
	secret, nt, err := c.h.rotate(func(t store.NPMToken) bool { return t.ID == id }, req.Now, Actor{By: "self", Addr: c.addr})
	switch {
	case errors.Is(err, ErrTokenRotated):
		writeErr(c.w, http.StatusConflict, "this token was rotated already; use the new one")
	case err != nil:
		writeErr(c.w, 500, "storage error")
	default:
		writeJSON(c.w, 200, TokenResponse{Token: secret, Info: nt})
	}
}

// AuditFilter selects audit entries; empty fields match everything.
type AuditFilter struct {
	Package string
	Token   string
	Action  string
	Since   time.Time
	Limit   int
}

// Audit returns audit entries, newest first.
func (h *Handler) Audit(f AuditFilter) ([]store.NPMAudit, error) {
	if f.Limit <= 0 || f.Limit > 10_000 {
		f.Limit = 1000
	}
	es, err := h.Store.NPMAuditLog(f.Limit, func(e store.NPMAudit) bool {
		return (f.Package == "" || e.Package == f.Package) &&
			(f.Token == "" || e.Token == f.Token) &&
			(f.Action == "" || e.Action == f.Action || strings.HasPrefix(e.Action, f.Action+".")) &&
			(f.Since.IsZero() || !e.Time.Before(f.Since))
	})
	if es == nil {
		es = []store.NPMAudit{}
	}
	return es, err
}

// Sweep is the registry's housekeeping, run by the daemon's periodic ticker: retention on
// every package (deleting versions whose grace period is over, and their tarball unless
// another version has the same bytes), audit entries older than 90 days or beyond the cap,
// rotated tokens past their grace, stale failed-auth counters and leftover temp files.
func (h *Handler) Sweep(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now, cfg := h.now(), h.config()
	pkgs, err := h.Store.NPMPackages()
	if err != nil {
		h.log().Error("npm: sweep", "err", err)
		return
	}
	for _, p := range pkgs {
		if ctx.Err() != nil {
			return
		}
		if err := h.retain(p.Name, now, cfg, true); err != nil {
			h.log().Error("npm: retention", "package", p.Name, "err", err)
		}
	}
	if _, err := h.Store.PruneNPMAudit(now.Add(-auditKeep), auditMax); err != nil {
		h.log().Error("npm: prune audit", "err", err)
	}
	h.Store.UpdateNPMTokens(func(ts []store.NPMToken) ([]store.NPMToken, []string, error) {
		var del []string
		for _, t := range ts {
			if t.ReplacedBy != "" && !now.Before(t.Expires) {
				del = append(del, t.ID)
			}
		}
		return nil, del, nil
	})
	h.limiter.prune(now)
	dir := filepath.Join(h.Dir, "tarballs")
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			if !strings.HasPrefix(e.Name(), tmpPrefix) {
				continue
			}
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > time.Hour {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
