package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/npm"
)

// npm token management and the registry's audit log. Admin-only: an npm token cannot reach
// these routes; it rotates itself at /npm/-/clowdr/token/rotate.

func (s *Server) npmActor(r *http.Request) npm.Actor {
	return npm.Actor{By: "admin:" + principal(r).TokenID, Addr: npm.ClientAddr(r)}
}

func (s *Server) npmOff(w http.ResponseWriter) bool {
	if s.NPM == nil {
		writeErr(w, 404, "this server has no npm registry")
		return true
	}
	return false
}

func (s *Server) listNPMTokens(w http.ResponseWriter, r *http.Request) {
	if s.npmOff(w) {
		return
	}
	ts, err := s.NPM.Tokens()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ts)
}

func (s *Server) createNPMToken(w http.ResponseWriter, r *http.Request) {
	if s.npmOff(w) {
		return
	}
	var req NPMTokenRequest
	if !decode(w, r, &req) {
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		d, err := config.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			writeErr(w, 400, "expires_in: want a duration such as 90d")
			return
		}
		ttl = d
	}
	secret, t, err := s.NPM.CreateToken(req.Name, req.Scopes, ttl, s.npmActor(r))
	switch {
	case errors.Is(err, npm.ErrTokenExists):
		writeErr(w, 409, "an npm token named "+req.Name+" exists: rotate or revoke it")
	case err != nil:
		writeErr(w, 400, err.Error())
	default:
		writeJSON(w, 200, npm.TokenResponse{Token: secret, Info: t})
	}
}

func (s *Server) revokeNPMToken(w http.ResponseWriter, r *http.Request) {
	if s.npmOff(w) {
		return
	}
	if err := s.NPM.RevokeToken(r.PathValue("name"), s.npmActor(r)); err != nil {
		writeErr(w, errStatus(err), "npm token "+r.PathValue("name")+": "+err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) rotateNPMToken(w http.ResponseWriter, r *http.Request) {
	if s.npmOff(w) {
		return
	}
	var req npm.RotateRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	secret, t, err := s.NPM.RotateToken(r.PathValue("name"), req.Now, s.npmActor(r))
	if err != nil {
		writeErr(w, errStatus(err), "npm token "+r.PathValue("name")+": "+err.Error())
		return
	}
	writeJSON(w, 200, npm.TokenResponse{Token: secret, Info: t})
}

func (s *Server) npmAudit(w http.ResponseWriter, r *http.Request) {
	if s.npmOff(w) {
		return
	}
	q := r.URL.Query()
	f := npm.AuditFilter{Package: q.Get("package"), Token: q.Get("token"), Action: q.Get("action")}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			writeErr(w, 400, "bad limit")
			return
		}
		f.Limit = n
	}
	if d := q.Get("since"); d != "" {
		dur, err := config.ParseDuration(d)
		if err != nil {
			writeErr(w, 400, "bad since")
			return
		}
		f.Since = time.Now().Add(-dur)
	}
	es, err := s.NPM.Audit(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, es)
}
