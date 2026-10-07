package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/deploy/fake"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/registry"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

type env struct {
	srv   *httptest.Server
	api   *Server
	admin *Client
	st    *store.Store
	d     *fake.Docker
	dir   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plat, _ := config.ParsePlatform([]byte(config.DefaultPlatform))
	d := fake.NewDocker()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := deploy.New(d, st, func() *config.Platform { return plat }, &ingress.Controller{Dir: dir, Nginx: &fake.Nginx{}}, log)
	eng.Prober = fake.Prober{D: d, Sick: map[string]bool{}}
	eng.CertExists = func(string) bool { return false }
	s := &Server{Store: st, Engine: eng, Log: log, Version: "test", RegistryKind: "fs"}
	s.Registry = &registry.Handler{
		Store: &registry.FSStore{Dir: filepath.Join(dir, "registry")}, Owners: st,
		Authorize: s.RegistryAuthorize, Log: log,
		OnTag: func(_ context.Context, repo, tag string, _ registry.Descriptor) {
			d.AddImage(registry.ImagePrefix + repo + ":" + tag)
		},
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	secret, tok := auth.New("", "", "bootstrap", true)
	if err := st.PutToken(tok); err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, api: s, admin: NewClient(srv.URL, secret, ""), st: st, d: d, dir: dir}
}

func (e *env) projectClient(t *testing.T, project string, domains ...string) *Client {
	t.Helper()
	ctx := context.Background()
	if err := e.admin.Do(ctx, "POST", "/v1/projects", ProjectRequest{Name: project, Domains: domains}, nil); err != nil {
		t.Fatal(err)
	}
	var tr TokenResponse
	if err := e.admin.Do(ctx, "POST", "/v1/tokens", TokenRequest{Project: project, Env: "prod"}, &tr); err != nil {
		t.Fatal(err)
	}
	return NewClient(e.srv.URL, tr.Token, "")
}

func push(t *testing.T, c *Client, repo string) {
	t.Helper()
	img, _ := random.Image(256, 2)
	ref, err := name.ParseReference(c.Host()+"/"+repo, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithAuth(&authn.Basic{Username: "x", Password: c.Token})); err != nil {
		t.Fatal(err)
	}
}

func svcSpec() spec.Service {
	return spec.Service{
		Name: "server", Port: 3000, Domains: []string{"api.example.com"},
		Health: spec.Health{Path: "/health", Interval: time.Millisecond, Timeout: time.Second},
		Drain:  time.Millisecond,
	}
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pc := e.projectClient(t, "shop", "example.com", "*.example.com")
	blog := e.projectClient(t, "blog", "example.org")

	push(t, pc, "shop/server:v1")
	var msgs []string
	ev, err := pc.Stream(ctx, "POST", Path("projects", "shop", "services", "server", "deploy"),
		DeployRequest{Spec: svcSpec(), Tag: "v1", Version: "abc123"}, func(ev Event) { msgs = append(msgs, ev.Message) })
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(msgs, "\n"))
	}
	if ev.Release == nil || ev.Release.Status != store.StatusActive || ev.Release.Author != "shop" {
		t.Fatalf("%+v", ev.Release)
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "traffic switched") {
		t.Fatal(msgs)
	}

	var ps []ServiceStatus
	if err := pc.Do(ctx, "GET", Path("projects", "shop", "services"), nil, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || len(ps[0].Containers) != 1 || ps[0].Containers[0].State == "missing" {
		t.Fatalf("%+v", ps)
	}

	var lines []LogLine
	if err := pc.Logs(ctx, Path("projects", "shop", "services", "server", "logs"), func(l LogLine) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0].Line, "hello from shop-server-r") {
		t.Fatalf("%+v", lines)
	}

	// env
	if err := pc.Do(ctx, "PATCH", Path("projects", "shop", "services", "server", "env"), EnvRequest{Set: map[string]string{"A": "1"}}, nil); err != nil {
		t.Fatal(err)
	}
	var keys []string
	pc.Do(ctx, "GET", Path("projects", "shop", "services", "server", "env"), nil, &keys)
	if strings.Join(keys, ",") != "A" {
		t.Fatal(keys)
	}

	// apply: route-only change reloads nginx without a release.
	s2 := svcSpec()
	s2.Routes = []spec.Route{{Path: "/ws", Timeout: time.Hour}}
	if _, err := pc.Stream(ctx, "POST", Path("projects", "shop", "apply"), ApplyRequest{Services: []spec.Service{s2}}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	var routes string
	pc.Do(ctx, "GET", Path("projects", "shop", "routes"), nil, &routes)
	if !strings.Contains(routes, "location /ws") {
		t.Fatal(routes)
	}
	var rels []store.Release
	pc.Do(ctx, "GET", Path("projects", "shop", "services", "server", "releases"), nil, &rels)
	if len(rels) != 1 {
		t.Fatalf("apply created a release: %d", len(rels))
	}

	// Isolation.
	var se *StatusError
	err = blog.Do(ctx, "GET", Path("projects", "shop", "services"), nil, nil)
	if !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("blog listed shop: %v", err)
	}
	if err := blog.Do(ctx, "GET", "/v1/projects", nil, nil); !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("project token listed projects: %v", err)
	}
	// blog cannot claim shop's domain, even in its own project.
	ks := svcSpec()
	ks.Domains = []string{"api.example.com"}
	push(t, blog, "blog/server:v1")
	if err := blog.Do(ctx, "POST", Path("projects", "blog", "services", "server", "deploy"), DeployRequest{Spec: ks, Tag: "v1"}, nil); err == nil {
		t.Fatal("blog deployed on shop's domain")
	}
	// Overlapping project domains are refused.
	if err := e.admin.Do(ctx, "POST", "/v1/projects", ProjectRequest{Name: "evil", Domains: []string{"api.example.com"}}, nil); err == nil {
		t.Fatal("overlapping domain accepted")
	}
	// Bad token.
	bad := NewClient(e.srv.URL, "clw_nope_nope", "")
	if err := bad.Do(ctx, "GET", "/v1/version", nil, nil); !errors.As(err, &se) || se.Status != 401 {
		t.Fatal(err)
	}
	var v Version
	if err := pc.Do(ctx, "GET", "/v1/version", nil, &v); err != nil || v.Principal != "shop" {
		t.Fatal(v, err)
	}
}

func TestIngressOnly(t *testing.T) {
	a := svcSpec()
	b := a
	b.Domains = []string{"x.example.com"}
	if !IngressOnly(a, b) {
		t.Fatal("domains only")
	}
	b.Memory = 1
	if IngressOnly(a, b) {
		t.Fatal("memory is not ingress")
	}
}

func TestPlatformStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	shop := e.projectClient(t, "shop", "example.com", "*.example.com")
	e.projectClient(t, "blog", "example.org")
	push(t, shop, "shop/server:v1")
	sp := svcSpec()
	sp.Memory, sp.Replicas = 128<<20, 2
	if _, err := shop.Stream(ctx, "POST", Path("projects", "shop", "services", "server", "deploy"), DeployRequest{Spec: sp, Tag: "v1"}, func(Event) {}); err != nil {
		t.Fatal(err)
	}

	var st PlatformStatus
	if err := e.admin.Do(ctx, "GET", "/v1/platform/status", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.Nginx.State != "missing" || len(st.Projects) != 2 {
		t.Fatalf("%+v", st)
	}
	for _, p := range st.Projects {
		if p.Name == "shop" && (p.Services != 1 || p.Running != 1 || p.Reserved != 256<<20) {
			t.Fatalf("%+v", p)
		}
	}
	var se *StatusError
	if err := shop.Do(ctx, "GET", "/v1/platform/status", nil, nil); !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("project token read platform status: %v", err)
	}
}

// A new project does not silently get the volumes of a removed one of the same name.
func TestProjectVolumesLeftover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	create := func(adopt bool) error {
		return e.admin.Do(ctx, "POST", "/v1/projects", ProjectRequest{Name: "shop", Domains: []string{"example.com"}, AdoptVolumes: adopt}, nil)
	}
	if err := create(false); err != nil {
		t.Fatal(err)
	}
	e.d.VolumeEnsure(ctx, "shop_data", map[string]string{"clowdr.project": "shop"})
	if err := e.admin.Do(ctx, "DELETE", "/v1/projects/shop", nil, nil); err != nil {
		t.Fatal(err)
	}
	var se *StatusError
	if err := create(false); !errors.As(err, &se) || se.Status != 409 || !strings.Contains(err.Error(), "shop_data") {
		t.Fatalf("leftover volumes: %v", err)
	}
	if err := create(true); err != nil {
		t.Fatalf("--adopt-volumes: %v", err)
	}
	if err := create(false); err != nil { // updating an existing project is not affected
		t.Fatal(err)
	}
	if err := e.admin.Do(ctx, "DELETE", "/v1/projects/shop?volumes=1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := create(false); err != nil {
		t.Fatalf("after rm --volumes: %v", err)
	}
	if err := e.admin.Do(ctx, "DELETE", "/v1/projects/nope", nil, nil); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("unknown project: %v", err)
	}
}
