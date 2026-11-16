package api

import (
	"context"
	"net/http"
	"time"

	"github.com/veksa/clowdr/internal/analytics"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/store"
)

// FeatureAnalytics is product analytics (docs/analytics.md): the routes under
// /v1/projects/{project}/analytics and the `analytics:` block of deploy.yml.
const FeatureAnalytics = "analytics"

// AnalyticsSecretRequest sets one secret of a project's analytics; an empty value removes it.
type AnalyticsSecretRequest struct {
	Value string `json:"value"`
}

// AnalyticsArchive is GET …/analytics/archive: presigned URLs of the raw days in the archive.
type AnalyticsArchive struct {
	URLs    []string `json:"urls"`
	Expires string   `json:"expires"`
}

// AnalyticsDigest is POST …/analytics/digest.
type AnalyticsDigest struct {
	Day  string `json:"day"`
	Text string `json:"text"`
	Sent bool   `json:"sent"`
}

func (s *Server) registerAnalytics(a, admin func(string, http.HandlerFunc)) {
	const base = "/v1/projects/{project}/analytics"
	a("POST "+base+"/query", s.analyticsQuery)
	a("GET "+base+"/archive", s.analyticsArchive)
	a("GET "+base+"/status", s.analyticsStatus)
	a("POST "+base+"/digest", s.analyticsDigest)
	a("GET "+base+"/config", s.analyticsGetConfig)
	a("PUT "+base+"/config", s.analyticsPutConfig)
	a("DELETE "+base+"/config", s.analyticsDeleteConfig)
	a("PUT "+base+"/secrets/{name}", s.analyticsSecret)
	// A rebuild rewrites the index and every rollup from the archive: the admin's call.
	admin("POST "+base+"/rebuild", s.analyticsRebuild)
}

// analyticsProject resolves the project of the request; it answers 501 on a server without
// analytics and 404 for a project that does not exist.
func (s *Server) analyticsProject(w http.ResponseWriter, r *http.Request) (*analytics.Project, bool) {
	if s.Analytics == nil {
		writeErr(w, 501, "this server has no analytics")
		return nil, false
	}
	name := r.PathValue("project")
	if _, err := s.Store.Project(name); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return nil, false
	}
	p, err := s.Analytics.Project(name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, false
	}
	return p, true
}

func (s *Server) analyticsQuery(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	var spec analytics.Spec
	if !decode(w, r, &spec) {
		return
	}
	res, err := p.Query(r.Context(), spec)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) analyticsArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	since, until, ok := window(w, r, 7*24*time.Hour)
	if !ok {
		return
	}
	if until.IsZero() {
		until = time.Now()
	}
	// The archive is the product's full raw history: a leaked link should not outlive the
	// query it was made for.
	const ttl = time.Hour
	urls, err := p.ArchiveURLs(r.Context(), since, until, ttl, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, AnalyticsArchive{URLs: urls, Expires: time.Now().Add(ttl).UTC().Format(time.RFC3339)})
}

func (s *Server) analyticsStatus(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.analyticsProject(w, r); ok {
		writeJSON(w, 200, p.Status())
	}
}

// analyticsDigest renders the daily digest (?day=, yesterday by default) and sends it unless
// ?dry_run=1.
func (s *Server) analyticsDigest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().UTC().AddDate(0, 0, -1).Format(analytics.DayLayout)
	}
	text, err := p.Digest(day)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	out := AnalyticsDigest{Day: day, Text: text}
	if r.URL.Query().Get("dry_run") != "1" {
		if err := p.SendDigest(r.Context(), day); err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		out.Sent = true
	}
	writeJSON(w, 200, out)
}

func (s *Server) analyticsGetConfig(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.analyticsProject(w, r); ok {
		writeJSON(w, 200, p.Config())
	}
}

// analyticsDeleteConfig removes the `analytics:` block: deploy.yml no longer has one.
func (s *Server) analyticsDeleteConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	changed := p.Config() != nil
	if err := p.DeleteConfig(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"changed": changed})
}

// analyticsPutConfig stores the `analytics:` block of deploy.yml.
func (s *Server) analyticsPutConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	var c config.AnalyticsConf
	if !decode(w, r, &c) {
		return
	}
	changed, err := p.SetConfig(c)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"changed": changed})
}

func (s *Server) analyticsSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	var req AnalyticsSecretRequest
	if !decode(w, r, &req) {
		return
	}
	if err := p.SetSecret(r.PathValue("name"), req.Value); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string][]string{"secrets": p.SecretsSet()})
}

// analyticsRebuild re-indexes the project from ?since= (the stream, then the archive) and
// streams its progress like a deploy.
func (s *Server) analyticsRebuild(w http.ResponseWriter, r *http.Request) {
	p, ok := s.analyticsProject(w, r)
	if !ok {
		return
	}
	since, err := analytics.ParseTime(r.URL.Query().Get("since"), time.Now())
	if err != nil || r.URL.Query().Get("since") == "" {
		writeErr(w, 400, "since: a day (2026-11-01) or an age (30d)")
		return
	}
	s.stream(w, r, func(ctx context.Context, progress deploy.Progress) (*store.Release, error) {
		return nil, p.Rebuild(ctx, since, func(msg string) { progress("%s", msg) })
	})
}
