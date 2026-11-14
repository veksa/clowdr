// Package apitest runs the clowdrd API in-process with in-memory docker and nginx, for tests
// of API clients such as clowctl.
package apitest

import (
	"context"
	"crypto/x509"
	"io"
	stdlog "log"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/deploy/fake"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/monitor"
	"github.com/veksa/clowdr/internal/npm"
	"github.com/veksa/clowdr/internal/registry"
	"github.com/veksa/clowdr/internal/store"
)

type Server struct {
	URL        string
	AdminToken string
	API        *api.Server
	Docker     *fake.Docker
	Store      *store.Store
	Logs       *logs.Store
	Events     *monitor.Events
	Dir        string
	// Cert is the self-signed certificate of a NewTLS server.
	Cert *x509.Certificate
}

func New(t testing.TB) *Server { return start(t, false) }

// NewTLS serves over HTTPS with a self-signed certificate, like clowdrd without a domain.
func NewTLS(t testing.TB) *Server { return start(t, true) }

func start(t testing.TB, tls bool) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plat, err := config.ParsePlatform([]byte(config.DefaultPlatform))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ls := &logs.Store{BufDir: filepath.Join(dir, "buf"), DataDir: filepath.Join(dir, "logs"), Log: log}
	if err := ls.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })

	d := fake.NewDocker()
	eng := deploy.New(d, st, func() *config.Platform { return plat }, &ingress.Controller{Dir: filepath.Join(dir, "nginx"), Nginx: &fake.Nginx{}}, log)
	eng.Prober = fake.Prober{D: d, Sick: map[string]bool{}}
	eng.CertExists = func(string) bool { return false }
	events := &monitor.Events{Logs: ls}
	eng.OnEvent = func(ctx context.Context, project, service, kind string, failed bool, msg string) {
		sev := monitor.Info
		if failed {
			sev = monitor.Error
		}
		events.Emit(ctx, monitor.Event{Project: project, Service: service, Kind: kind, Severity: sev, Message: msg}, "")
	}

	s := &api.Server{Store: st, Engine: eng, Log: log, Version: "test", RegistryKind: "fs", Logs: ls, Events: events, StateDir: dir}
	s.Registry = &registry.Handler{
		Store: &registry.FSStore{Dir: filepath.Join(dir, "registry")}, Owners: st,
		Authorize: s.RegistryAuthorize, Log: log,
		OnTag: func(_ context.Context, repo, tag string, _ registry.Descriptor) {
			d.AddImage(registry.ImagePrefix + repo + ":" + tag)
		},
	}
	s.NPM = &npm.Handler{Store: st, Dir: filepath.Join(dir, "npm"), Log: log,
		Config: func() config.NPMConf { return plat.NPM }}
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0) // rejected TLS handshakes are expected in tests
	if tls {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	secret, tok := auth.New("", "", "bootstrap", true)
	if err := st.PutToken(tok); err != nil {
		t.Fatal(err)
	}
	return &Server{URL: srv.URL, AdminToken: secret, API: s, Docker: d, Store: st, Logs: ls, Events: events, Dir: dir, Cert: srv.Certificate()}
}

// Project creates a project with its allowed domains and returns a token for it.
func (s *Server) Project(t testing.TB, name string, domains ...string) string {
	t.Helper()
	ctx := context.Background()
	admin := api.NewClient(s.URL, s.AdminToken, "")
	if err := admin.Do(ctx, "POST", "/v1/projects", api.ProjectRequest{Name: name, Domains: domains}, nil); err != nil {
		t.Fatal(err)
	}
	var tr api.TokenResponse
	if err := admin.Do(ctx, "POST", "/v1/tokens", api.TokenRequest{Project: name, Env: "prod"}, &tr); err != nil {
		t.Fatal(err)
	}
	return tr.Token
}
