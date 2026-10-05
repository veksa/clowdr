package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

type fakeContainer struct {
	cfg     runtime.ContainerConfig
	running bool
	exit    int
}

type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	images     map[string]bool
	removed    []string
	memTotal   int64
	// crash makes containers of these images exit right after start.
	crash map[string]bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]*fakeContainer{}, images: map[string]bool{}, crash: map[string]bool{}}
}

func (f *fakeDocker) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{MemTotal: f.memTotal}, nil
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string) (runtime.ImageInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[ref] {
		return runtime.ImageInspect{}, &runtime.APIError{Status: 404}
	}
	return runtime.ImageInspect{ID: ref}, nil
}

func (f *fakeDocker) ImagePull(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = true
	return nil
}

func (f *fakeDocker) ImageRemove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, ref)
	f.removed = append(f.removed, ref)
	return nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, c runtime.ContainerConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[c.Name]; ok {
		return "", &runtime.APIError{Status: 409, Message: "name in use"}
	}
	f.containers[c.Name] = &fakeContainer{cfg: c}
	return c.Name, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return &runtime.APIError{Status: 404}
	}
	c.running = !f.crash[c.cfg.Image]
	if !c.running {
		c.exit = 1
	}
	return nil
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		c.running = false
	}
	return nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	return nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (runtime.ContainerInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return runtime.ContainerInspect{}, &runtime.APIError{Status: 404, Message: "no such container"}
	}
	var ci runtime.ContainerInspect
	ci.Name = "/" + id
	ci.Config.Image = c.cfg.Image
	ci.Config.Labels = c.cfg.Labels
	ci.State.Running = c.running
	ci.State.ExitCode = c.exit
	ci.NetworkSettings.Networks = map[string]struct {
		IPAddress string
		Aliases   []string
	}{c.cfg.Network: {IPAddress: id}}
	return ci, nil
}

func (f *fakeDocker) ContainerLogs(context.Context, string, bool, string, time.Time) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDocker) Containers(_ context.Context, labels ...string) ([]runtime.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.ContainerSummary
	for name, c := range f.containers {
		match := true
		for _, l := range labels {
			k, v, hasV := strings.Cut(l, "=")
			if got, ok := c.cfg.Labels[k]; !ok || hasV && got != v {
				match = false
			}
		}
		if match {
			state := "exited"
			if c.running {
				state = "running"
			}
			out = append(out, runtime.ContainerSummary{ID: name, Names: []string{"/" + name}, State: state, Labels: c.cfg.Labels})
		}
	}
	return out, nil
}

func (f *fakeDocker) NetworkEnsure(context.Context, string, map[string]string) error { return nil }
func (f *fakeDocker) NetworkConnect(context.Context, string, string, []string) error { return nil }
func (f *fakeDocker) VolumeEnsure(context.Context, string, map[string]string) error  { return nil }
func (f *fakeDocker) Exec(context.Context, string, []string) (string, int, error)    { return "", 0, nil }

func (f *fakeDocker) running(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, c := range f.containers {
		if strings.HasPrefix(n, prefix) && c.running {
			out = append(out, n)
		}
	}
	return out
}

// fakeProber: a container is healthy unless its image is in sick.
type fakeProber struct {
	d    *fakeDocker
	sick map[string]bool
}

func (p fakeProber) Probe(_ context.Context, u string) error {
	pu, _ := url.Parse(u)
	name := pu.Hostname()
	p.d.mu.Lock()
	defer p.d.mu.Unlock()
	c := p.d.containers[name]
	if c == nil || p.sick[c.cfg.Image] {
		return errors.New("connection refused")
	}
	return nil
}

type okNginx struct{ reloads int }

func (n *okNginx) Test(context.Context) (string, error) { return "", nil }
func (n *okNginx) Reload(context.Context) error         { n.reloads++; return nil }

type fixture struct {
	e     *Engine
	d     *fakeDocker
	st    *store.Store
	sick  map[string]bool
	dir   string
	nginx *okNginx
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plat, _ := config.ParsePlatform([]byte(config.DefaultPlatform))
	plat.Addons["redis"] = config.AddonConf{Image: "redis:7-alpine", Port: 6379, Allow: []string{"shop"}}
	d := newFakeDocker()
	ng := &okNginx{}
	e := New(d, st, func() *config.Platform { return plat }, &ingress.Controller{Dir: dir, Nginx: ng}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sick := map[string]bool{}
	e.Prober = fakeProber{d: d, sick: sick}
	e.CertExists = func(string) bool { return false }
	if err := st.PutProject(store.Project{Name: "shop", Domains: []string{"example.com", "*.example.com"}}); err != nil {
		t.Fatal(err)
	}
	return &fixture{e: e, d: d, st: st, sick: sick, dir: dir, nginx: ng}
}

func (f *fixture) push(img string) string {
	ref := "clowdr.local/shop/server:" + img
	f.d.images[ref] = true
	return ref
}

func serverSpec() spec.Service {
	s := spec.Service{
		Name: "server", Port: 3000, Memory: 100 << 20,
		Domains: []string{"api.example.com"},
		Health:  spec.Health{Path: "/health", Interval: time.Millisecond, Timeout: 50 * time.Millisecond, Successes: 2},
		Drain:   time.Millisecond,
	}
	s.Defaults()
	return s
}

func (f *fixture) deploy(t *testing.T, img string, s spec.Service) (store.Release, error) {
	t.Helper()
	var log []string
	rel, err := f.e.Deploy(context.Background(), Request{
		Project: "shop", Spec: s, Image: f.push(img), Version: img, Author: "test",
	}, func(format string, a ...any) { log = append(log, fmt.Sprintf(format, a...)) })
	f.e.WaitDrains()
	return rel, err
}

func (f *fixture) conf(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(f.dir, "conf.d", "shop.conf"))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestDeploySwitchAndRollback(t *testing.T) {
	f := newFixture(t)
	r1, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	if r1.Status != store.StatusActive || !strings.Contains(f.conf(t), "shop-server-r1-1:3000") {
		t.Fatalf("r1: %+v\n%s", r1, f.conf(t))
	}

	r2, err := f.deploy(t, "v2", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	conf := f.conf(t)
	if !strings.Contains(conf, fmt.Sprintf("shop-server-r%d-1:3000", r2.ID)) || strings.Contains(conf, "r1-1") {
		t.Fatalf("not switched:\n%s", conf)
	}
	if got := f.d.running("shop-server"); len(got) != 1 {
		t.Fatalf("old not drained: %v", got)
	}
	svc, _ := f.st.Service("shop", "server")
	if svc.Current != r2.ID || svc.Previous != r1.ID {
		t.Fatalf("%+v", svc)
	}

	// Unhealthy release: old keeps serving, new containers are gone.
	f.sick["clowdr.local/shop/server:v3"] = true
	r3, err := f.deploy(t, "v3", serverSpec())
	if !errors.Is(err, ErrHealth) || r3.Status != store.StatusFailed {
		t.Fatalf("want health failure, got %v %+v", err, r3)
	}
	if !strings.Contains(f.conf(t), fmt.Sprintf("r%d-1", r2.ID)) {
		t.Fatal("config changed by a failed deploy")
	}
	if got := f.d.running("shop-server"); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("r%d", r2.ID)) {
		t.Fatalf("running: %v", got)
	}

	// Crashing release.
	f.d.crash["clowdr.local/shop/server:v4"] = true
	if _, err := f.deploy(t, "v4", serverSpec()); !errors.Is(err, ErrHealth) {
		t.Fatal(err)
	}

	// Rollback to r1's image.
	rb, err := f.e.Rollback(context.Background(), "shop", "server", 0, "test", func(string, ...any) {})
	f.e.WaitDrains()
	if err != nil {
		t.Fatal(err)
	}
	if rb.Image != r1.Image || !rb.Rollback {
		t.Fatalf("%+v", rb)
	}
	if _, err := f.e.Rollback(context.Background(), "shop", "server", r3.ID, "test", func(string, ...any) {}); err == nil {
		t.Fatal("rolled back to a failed release")
	}
}

func TestGC(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"v1", "v2", "v3", "v4"} {
		if _, err := f.deploy(t, v, serverSpec()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.d.removed, ",") != "clowdr.local/shop/server:v1,clowdr.local/shop/server:v2" {
		t.Fatalf("removed %v", f.d.removed)
	}
	if !f.d.images["clowdr.local/shop/server:v3"] || !f.d.images["clowdr.local/shop/server:v4"] {
		t.Fatal("current or previous image removed")
	}
}

func TestReplicas(t *testing.T) {
	f := newFixture(t)
	s := serverSpec()
	s.Replicas = 3
	r, err := f.deploy(t, "v1", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Containers) != 3 || strings.Count(f.conf(t), "resolve max_fails") != 3 {
		t.Fatalf("%v\n%s", r.Containers, f.conf(t))
	}
}

func TestChecks(t *testing.T) {
	f := newFixture(t)
	s := serverSpec()
	s.Domains = []string{"example.org"}
	if _, err := f.deploy(t, "v1", s); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("foreign domain: %v", err)
	}
	s = serverSpec()
	s.NginxExtra = "add_header X 1;"
	if _, err := f.deploy(t, "v1", s); err == nil {
		t.Fatal("nginx_extra without permission")
	}
	if _, err := f.e.Deploy(context.Background(), Request{Project: "blog", Spec: serverSpec(), Image: f.push("x")}, nil); err == nil {
		t.Fatal("unknown project")
	}
	// Same domain from two services.
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	other := serverSpec()
	other.Name = "admin"
	if _, err := f.deploy(t, "v1", other); err == nil || !strings.Contains(err.Error(), "already served") {
		t.Fatalf("domain taken twice: %v", err)
	}
	// Missing image.
	if _, err := f.e.Deploy(context.Background(), Request{Project: "shop", Spec: serverSpec(), Image: "clowdr.local/shop/server:nope"}, nil); err == nil {
		t.Fatal("missing image")
	}
}

func TestQuotaAndStopFirst(t *testing.T) {
	f := newFixture(t)
	p, _ := f.st.Project("shop")
	p.Memory = 150 << 20
	f.st.PutProject(p)
	s := serverSpec() // 100m
	if _, err := f.deploy(t, "v1", s); err != nil {
		t.Fatal(err)
	}
	// 100m old + 100m new > 150m: stop-first instead of failing.
	f.sick["clowdr.local/shop/server:v2"] = true
	_, err := f.deploy(t, "v2", s)
	if !errors.Is(err, ErrHealth) {
		t.Fatal(err)
	}
	// Old release was stopped and started again after the failure.
	if got := f.d.running("shop-server-r1"); len(got) != 1 {
		t.Fatalf("old not restored: %v", got)
	}
	big := serverSpec()
	big.Memory = 200 << 20
	if _, err := f.deploy(t, "v3", big); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
}

func TestAddonAndStopStart(t *testing.T) {
	f := newFixture(t)
	s := serverSpec()
	s.Uses = []string{"redis"}
	r, err := f.deploy(t, "v1", s)
	if err != nil {
		t.Fatal(err)
	}
	c := f.d.containers[r.Containers[0]]
	if !strings.Contains(strings.Join(c.cfg.Env, " "), "REDIS_URL=redis://redis:6379") {
		t.Fatal(c.cfg.Env)
	}
	if rc := f.d.containers["shop-redis"]; rc == nil || !rc.running || rc.cfg.Aliases[0] != "redis" {
		t.Fatal("redis addon not running")
	}
	ctx := context.Background()
	if err := f.e.Stop(ctx, "shop", "server"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.conf(t), "127.0.0.1:1 down") || len(f.d.running("shop-server")) != 0 {
		t.Fatal("not stopped")
	}
	if err := f.e.Start(ctx, "shop", "server", func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.conf(t), "shop-server-r1-1:3000") {
		t.Fatal("not started")
	}
	if err := f.e.Remove(ctx, "shop", "server"); err != nil {
		t.Fatal(err)
	}
	if f.conf(t) != "" || len(f.d.running("shop-server")) != 0 {
		t.Fatal("not removed")
	}
}
