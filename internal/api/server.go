package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/monitor"
	"github.com/veksa/clowdr/internal/registry"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/sdk"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

type Server struct {
	Store    *store.Store
	Engine   *deploy.Engine
	Registry *registry.Handler
	Log      *slog.Logger
	Version  string
	// RegistryKind is shown in `clowctl version` ("containerd" or "fs").
	RegistryKind string
	// Logs is the log store; nil falls back to docker logs.
	Logs *logs.Store
	// Events is the events journal; nil disables `clowctl events`.
	Events *monitor.Events
	// StateDir is used for the host disk figures in `clowctl top`.
	StateDir string
}

type ctxKey struct{}

func principal(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(ctxKey{}).(auth.Principal)
	return p
}

// Handler returns the full HTTP handler: /v1 API, /v2 registry and ACME challenges.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	a := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authed(h, false)) }
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authed(h, true)) }

	a("GET /v1/version", s.version)

	admin("GET /v1/projects", s.listProjects)
	admin("POST /v1/projects", s.createProject)
	admin("DELETE /v1/projects/{project}", s.deleteProject)
	admin("GET /v1/tokens", s.listTokens)
	admin("POST /v1/tokens", s.createToken)
	admin("DELETE /v1/tokens/{id}", s.deleteToken)
	admin("GET /v1/platform/status", s.platformStatus)
	admin("POST /v1/platform/reconcile", s.reconcile)
	admin("POST /v1/gc", s.gc)

	a("GET /v1/projects/{project}", s.getProject)
	a("GET /v1/projects/{project}/services", s.listServices)
	a("POST /v1/projects/{project}/apply", s.apply)
	a("GET /v1/projects/{project}/routes", s.routes)
	a("GET /v1/projects/{project}/services/{service}", s.getService)
	a("DELETE /v1/projects/{project}/services/{service}", s.removeService)
	a("POST /v1/projects/{project}/services/{service}/deploy", s.deploy)
	a("POST /v1/projects/{project}/services/{service}/rollback", s.rollback)
	a("POST /v1/projects/{project}/services/{service}/restart", s.restart)
	a("POST /v1/projects/{project}/services/{service}/stop", s.stop)
	a("POST /v1/projects/{project}/services/{service}/start", s.start)
	a("GET /v1/projects/{project}/services/{service}/releases", s.releases)
	a("GET /v1/projects/{project}/services/{service}/logs", s.logs)
	a("GET /v1/projects/{project}/services/{service}/env", s.getEnv)
	a("PATCH /v1/projects/{project}/services/{service}/env", s.patchEnv)
	a("GET /v1/projects/{project}/access", s.access)
	a("GET /v1/projects/{project}/traces", s.traces)
	a("GET /v1/projects/{project}/traces/{trace}", s.traceOne)
	a("GET /v1/projects/{project}/trace-records", s.traceRecords)
	a("GET /v1/projects/{project}/metrics", s.metricsList)
	a("GET /v1/projects/{project}/metrics/{name}", s.metricQuery)
	a("GET /v1/projects/{project}/events", s.events)
	s.registerOps(a)

	if s.Registry != nil {
		mux.Handle("/v2/", s.Registry)
	}
	// Client packages (docs/tracing.md): open source, no token.
	mux.Handle("GET /sdk/node/", sdk.Handler(writeJSON))
	mux.HandleFunc("GET "+ingress.ACMEChallenge, func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, ingress.ACMEChallenge)
		if strings.ContainsAny(token, "/\\.") || token == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(ACMEDir, token))
	})
	return s.logRequests(mux)
}

// ACMEDir holds HTTP-01 challenge responses.
var ACMEDir = "/var/lib/clowdr/acme-challenge"

func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/v2/") && sw.status < 400 {
			return // registry is chatty
		}
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TokenFromRequest reads "Authorization: Bearer <token>" or basic auth (password = token).
func TokenFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	if _, p, ok := r.BasicAuth(); ok {
		return p
	}
	return ""
}

// RegistryAuthorize adapts token auth for the registry handler.
func (s *Server) RegistryAuthorize(r *http.Request) (string, error) {
	p, err := auth.Check(s.Store, TokenFromRequest(r))
	if err != nil {
		return "", err
	}
	if p.Admin {
		return "*", nil
	}
	return p.Project, nil
}

func (s *Server) authed(h http.HandlerFunc, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := auth.Check(s.Store, TokenFromRequest(r))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		if adminOnly && !p.Admin {
			writeErr(w, http.StatusForbidden, "admin token required")
			return
		}
		if project := r.PathValue("project"); project != "" && !p.CanProject(project) {
			writeErr(w, http.StatusForbidden, "token has no access to project "+project)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, Error{Error: msg})
}

func errStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, runtime.ErrNotFound):
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return false
	}
	return true
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	v := Version{Version: s.Version, Registry: s.RegistryKind, Principal: principal(r).Name()}
	if info, err := s.Engine.Docker.Info(r.Context()); err == nil {
		v.Docker = info.ServerVersion
	}
	writeJSON(w, 200, v)
}

// Projects and tokens

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.Store.Projects()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ps)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req ProjectRequest
	if !decode(w, r, &req) {
		return
	}
	if !spec.ValidName(req.Name) {
		writeErr(w, 400, "invalid project name")
		return
	}
	for _, d := range req.Domains {
		if !spec.ValidDomain(d) {
			writeErr(w, 400, "invalid domain "+d)
			return
		}
	}
	if req.Name == "clowdr" {
		writeErr(w, 400, "project name clowdr is reserved")
		return
	}
	// A domain pattern belongs to one project only.
	others, _ := s.Store.Projects()
	for _, o := range others {
		if o.Name == req.Name {
			continue
		}
		// Container names are <project>-<service>-r<n>-<i> and <project>-<addon>: with projects
		// a and a-b, a's service b-c and a-b's service c would claim the same names.
		if strings.HasPrefix(o.Name, req.Name+"-") || strings.HasPrefix(req.Name, o.Name+"-") {
			writeErr(w, 409, fmt.Sprintf("project name %s clashes with project %s: one must not start with the other followed by '-'", req.Name, o.Name))
			return
		}
		for _, d := range req.Domains {
			for _, od := range o.Domains {
				if overlaps(d, od) {
					writeErr(w, 409, fmt.Sprintf("domain %s overlaps %s of project %s", d, od, o.Name))
					return
				}
			}
		}
	}
	plat := s.Engine.Platform()
	for _, a := range req.Addons {
		if _, ok := plat.Addons[a]; !ok {
			writeErr(w, 400, "addon "+a+" is not configured in platform.yml")
			return
		}
	}
	p, err := s.Store.Project(req.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, 500, err.Error())
		return
	}
	if err != nil && !req.AdoptVolumes { // a new project
		left, lerr := s.Engine.LeftoverVolumes(r.Context(), req.Name)
		if lerr != nil {
			writeErr(w, 500, lerr.Error())
			return
		}
		if len(left) > 0 {
			writeErr(w, 409, fmt.Sprintf("volumes of a removed project %s are still on the host (%s): pass --adopt-volumes to give them to the new project, or remove them first (docker volume rm %s)",
				req.Name, strings.Join(left, ", "), strings.Join(left, " ")))
			return
		}
	}
	p.Name, p.Domains, p.Addons, p.Memory, p.NginxExtra = req.Name, req.Domains, req.Addons, req.Memory, req.NginxExtra
	if err := s.Store.PutProject(p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, p)
}

// overlaps reports whether two domain patterns can match the same host.
func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	return auth.DomainAllowed([]string{a}, b) || auth.DomainAllowed([]string{b}, a)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if _, err := s.Store.Project(project); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if err := s.Engine.RemoveProject(r.Context(), project, r.URL.Query().Get("volumes") == "1"); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.Store.DeleteProject(project); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.Engine.RenderAll(r.Context())
	w.WriteHeader(204)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.Tokens()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for i := range ts {
		ts[i].Hash = ""
	}
	writeJSON(w, 200, ts)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req TokenRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Admin == (req.Project != "") {
		writeErr(w, 400, "token needs either --project or --admin")
		return
	}
	if req.Project != "" {
		if _, err := s.Store.Project(req.Project); err != nil {
			writeErr(w, 404, "project "+req.Project+" does not exist")
			return
		}
	}
	secret, t := auth.New(req.Project, req.Env, req.Name, req.Admin)
	if err := s.Store.PutToken(t); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.Hash = ""
	writeJSON(w, 200, TokenResponse{Token: secret, Info: t})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == principal(r).TokenID {
		writeErr(w, 400, "refusing to revoke the token used for this request")
		return
	}
	if err := s.Store.DeleteToken(id); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.EnsureNginx(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.Engine.RenderAll(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// Services

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("project"))
	if err != nil {
		writeErr(w, errStatus(err), "project not found")
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	svcs, err := s.Store.Services(project)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	withStats := r.URL.Query().Get("stats") == "1"
	out := make([]ServiceStatus, len(svcs))
	var wg sync.WaitGroup
	for i, v := range svcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.status(r.Context(), v, withStats)
		}()
	}
	wg.Wait()
	writeJSON(w, 200, out)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Service(r.PathValue("project"), r.PathValue("service"))
	if err != nil {
		writeErr(w, errStatus(err), "service not found")
		return
	}
	writeJSON(w, 200, s.status(r.Context(), v, true))
}

type statser interface {
	Stats(ctx context.Context, id string) (runtime.Stats, error)
}

func (s *Server) status(ctx context.Context, v store.Service, withStats bool) ServiceStatus {
	st := ServiceStatus{Name: v.Name, Stopped: v.Stopped, Replicas: v.Spec.Replicas, Memory: v.Spec.Memory, Domains: v.Spec.Domains, Spec: v.Spec}
	rel, err := s.Store.Release(v.Project, v.Name, v.Current)
	if err != nil {
		return st
	}
	st.Release = &rel
	for _, c := range rel.Containers {
		cs := ContainerStatus{Name: c, State: "missing"}
		if ci, err := s.Engine.Docker.ContainerInspect(ctx, c); err == nil {
			cs.State = ci.State.Status
			cs.Started = ci.State.StartedAt
			cs.Restarts = ci.RestartCount
			cs.OOM = ci.State.OOMKilled
			if ci.State.Health != nil {
				cs.Health = ci.State.Health.Status
			}
			if sd, ok := s.Engine.Docker.(statser); ok && withStats && ci.State.Running {
				if stats, err := sd.Stats(ctx, c); err == nil {
					cs.MemUsage, cs.CPU = stats.MemoryUsage, stats.CPUPercent
				}
			}
		}
		st.Containers = append(st.Containers, cs)
	}
	return st
}

func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(filepath.Join(s.Engine.Ingress.Dir, filepath.FromSlash(ingress.ProjectFile(r.PathValue("project")))))
	if err != nil {
		writeErr(w, 404, "no routes")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.Releases(r.PathValue("project"), r.PathValue("service"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rs == nil {
		if _, err := s.Store.Service(r.PathValue("project"), r.PathValue("service")); err != nil {
			writeErr(w, errStatus(err), "service "+r.PathValue("service")+": "+err.Error())
			return
		}
		rs = []store.Release{}
	}
	writeJSON(w, 200, rs)
}

// stream runs fn and writes its progress as NDJSON events.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, progress deploy.Progress) (*store.Release, error)) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	send := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		ev.Time = time.Now().UTC()
		enc.Encode(ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// A deploy continues if the CLI disconnects: stopping halfway helps nobody.
	ctx := context.WithoutCancel(r.Context())
	rel, err := fn(ctx, func(format string, args ...any) { send(Event{Message: fmt.Sprintf(format, args...)}) })
	ev := Event{Done: true, Release: rel}
	if err != nil {
		ev.Error = err.Error()
	}
	send(ev)
}

func author(r *http.Request, given string) string {
	p := principal(r)
	if given == "" {
		return p.Name()
	}
	return given
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	project, service := r.PathValue("project"), r.PathValue("service")
	var req DeployRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Spec.Name != service {
		writeErr(w, 400, "spec name does not match the service in the URL")
		return
	}
	image := req.Image
	switch {
	case req.Tag != "" && req.Image != "":
		writeErr(w, 400, "tag and image are exclusive")
		return
	case req.Tag != "" && !registry.ValidTag(req.Tag):
		writeErr(w, 400, "invalid tag "+req.Tag)
		return
	case req.Tag != "":
		image = registry.ImagePrefix + project + "/" + service + ":" + req.Tag
	case req.Image == "":
		writeErr(w, 400, "tag or image is required")
		return
	}
	if err := s.Engine.CheckSpec(project, withDefaults(req.Spec)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := deploy.CheckImage(project, image); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	s.stream(w, r, func(ctx context.Context, p deploy.Progress) (*store.Release, error) {
		rel, err := s.Engine.Deploy(ctx, deploy.Request{
			Project: project, Spec: req.Spec, Image: image, Version: req.Version, Author: author(r, req.Author),
		}, p)
		return &rel, err
	})
}

func withDefaults(s spec.Service) spec.Service {
	s.Defaults()
	return s
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var req RollbackRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	s.stream(w, r, func(ctx context.Context, p deploy.Progress) (*store.Release, error) {
		rel, err := s.Engine.Rollback(ctx, r.PathValue("project"), r.PathValue("service"), req.To, author(r, req.Author), p)
		return &rel, err
	})
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	s.stream(w, r, func(ctx context.Context, p deploy.Progress) (*store.Release, error) {
		rel, err := s.Engine.Restart(ctx, r.PathValue("project"), r.PathValue("service"), author(r, req.Author), p)
		return &rel, err
	})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.Stop(r.Context(), r.PathValue("project"), r.PathValue("service")); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	s.stream(w, r, func(ctx context.Context, p deploy.Progress) (*store.Release, error) {
		return nil, s.Engine.Start(ctx, r.PathValue("project"), r.PathValue("service"), p)
	})
}

func (s *Server) removeService(w http.ResponseWriter, r *http.Request) {
	project, service := r.PathValue("project"), r.PathValue("service")
	if _, err := s.Store.Service(project, service); err != nil {
		writeErr(w, 404, "service not found")
		return
	}
	if err := s.Engine.Remove(r.Context(), project, service); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// apply updates specs without a new image. Changes that only affect nginx are applied by a
// reload; anything else redeploys the current image with the new spec (health-gated).
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req ApplyRequest
	if !decode(w, r, &req) {
		return
	}
	for _, sp := range req.Services {
		if err := s.Engine.CheckSpec(project, withDefaults(sp)); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	s.stream(w, r, func(ctx context.Context, p deploy.Progress) (*store.Release, error) {
		for _, sp := range req.Services {
			sp.Defaults()
			cur, err := s.Store.Service(project, sp.Name)
			if errors.Is(err, store.ErrNotFound) {
				p("%s: not deployed yet, skipped", sp.Name)
				continue
			} else if err != nil {
				return nil, err
			}
			switch {
			case reflect.DeepEqual(cur.Spec, sp):
				p("%s: unchanged", sp.Name)
			case IngressOnly(cur.Spec, sp):
				if _, err := s.Store.UpdateService(project, sp.Name, func(v *store.Service) error { v.Spec = sp; return nil }); err != nil {
					return nil, err
				}
				if err := s.Engine.RenderProject(ctx, project); err != nil {
					s.Store.UpdateService(project, sp.Name, func(v *store.Service) error { v.Spec = cur.Spec; return nil })
					return nil, err
				}
				p("%s: routes updated (nginx reloaded)", sp.Name)
			default:
				rel, err := s.Store.Release(project, sp.Name, cur.Current)
				if err != nil {
					return nil, fmt.Errorf("%s: no active release", sp.Name)
				}
				p("%s: redeploying r%d with the new spec", sp.Name, rel.ID)
				if _, err := s.Engine.Deploy(ctx, deploy.Request{
					Project: project, Spec: sp, Image: rel.Image, Digest: rel.Digest,
					Version: rel.Version, Author: author(r, req.Author),
				}, p); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	})
}

// IngressOnly reports whether a and b differ only in fields that live in nginx.
func IngressOnly(a, b spec.Service) bool {
	a.Domains, a.Routes, a.TLS, a.MaxBody, a.Timeout, a.NginxExtra = b.Domains, b.Routes, b.TLS, b.MaxBody, b.Timeout, b.NginxExtra
	return reflect.DeepEqual(a, b)
}

// Env

func (s *Server) getEnv(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Service(r.PathValue("project"), r.PathValue("service"))
	if err != nil {
		writeErr(w, errStatus(err), "service not found")
		return
	}
	keys := make([]string, 0, len(v.Env))
	for k := range v.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if r.URL.Query().Get("values") == "1" {
		writeJSON(w, 200, v.Env)
		return
	}
	writeJSON(w, 200, keys)
}

func (s *Server) patchEnv(w http.ResponseWriter, r *http.Request) {
	project, service := r.PathValue("project"), r.PathValue("service")
	var req EnvRequest
	if !decode(w, r, &req) {
		return
	}
	for k := range req.Set {
		if !spec.ValidEnvName(k) || strings.HasPrefix(k, "CLOWDR_") {
			writeErr(w, 400, "invalid env name "+k)
			return
		}
	}
	_, err := s.Store.UpdateService(project, service, func(v *store.Service) error {
		if v.Spec.Name == "" {
			// Allow setting env before the first deploy.
			if !spec.ValidName(service) {
				return errors.New("invalid service name")
			}
		}
		if v.Env == nil {
			v.Env = map[string]string{}
		}
		for k, val := range req.Set {
			v.Env[k] = val
		}
		for _, k := range req.Unset {
			delete(v.Env, k)
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}

// dockerLogs reads `docker logs` (json-file driver; development and tests).
func (s *Server) dockerLogs(w http.ResponseWriter, r *http.Request) {
	project, service := r.PathValue("project"), r.PathValue("service")
	q := r.URL.Query()
	v, err := s.Store.Service(project, service)
	if err != nil {
		writeErr(w, 404, "service not found")
		return
	}
	relID := v.Current
	if id := q.Get("release"); id != "" {
		n, err := strconv.ParseUint(strings.TrimPrefix(id, "r"), 10, 64)
		if err != nil {
			writeErr(w, 400, "bad release")
			return
		}
		relID = n
	}
	rel, err := s.Store.Release(project, service, relID)
	if err != nil {
		writeErr(w, 404, "release not found")
		return
	}
	follow := q.Get("follow") == "1"
	tail := q.Get("tail")
	if tail == "" {
		tail = "100"
	}
	var since time.Time
	if d := q.Get("since"); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			writeErr(w, 400, "bad since")
			return
		}
		since = time.Now().Add(-dur)
	}
	var grep string
	if g := q.Get("grep"); g != "" {
		grep = g
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	var wg sync.WaitGroup
	for _, c := range rel.Containers {
		rc, err := s.Engine.Docker.ContainerLogs(r.Context(), c, follow, tail, since)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer rc.Close()
			runtime.Demux(rc, func(stderr bool, line string) {
				ts, msg, ok := strings.Cut(line, " ")
				if !ok {
					ts, msg = "", line
				}
				if grep != "" && !strings.Contains(msg, grep) {
					return
				}
				mu.Lock()
				enc.Encode(LogLine{Time: ts, Container: c, Stderr: stderr, Line: msg})
				if follow && flusher != nil {
					flusher.Flush()
				}
				mu.Unlock()
			})
		}()
	}
	wg.Wait()
}
