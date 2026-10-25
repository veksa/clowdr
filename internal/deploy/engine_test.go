package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy/fake"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

type fixture struct {
	e     *Engine
	d     *fake.Docker
	st    *store.Store
	sick  map[string]bool
	dir   string
	nginx *fake.Nginx
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
	d := fake.NewDocker()
	ng := &fake.Nginx{}
	e := New(d, st, func() *config.Platform { return plat }, &ingress.Controller{Dir: dir, Nginx: ng}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sick := map[string]bool{}
	e.Prober = fake.Prober{D: d, Sick: sick}
	e.CertExists = func(string) bool { return false }
	if err := st.PutProject(store.Project{Name: "shop", Domains: []string{"example.com", "*.example.com"}}); err != nil {
		t.Fatal(err)
	}
	return &fixture{e: e, d: d, st: st, sick: sick, dir: dir, nginx: ng}
}

func (f *fixture) push(img string) string {
	ref := "clowdr.local/shop/server:" + img
	f.d.Local[ref] = true
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
	if got := f.d.RunningWith("shop-server"); len(got) != 1 {
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
	if got := f.d.RunningWith("shop-server"); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("r%d", r2.ID)) {
		t.Fatalf("running: %v", got)
	}

	// Crashing release.
	f.d.Crash["clowdr.local/shop/server:v4"] = true
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
	if strings.Join(f.d.Removed, ",") != "clowdr.local/shop/server:v1,clowdr.local/shop/server:v2" {
		t.Fatalf("removed %v", f.d.Removed)
	}
	if !f.d.Local["clowdr.local/shop/server:v3"] || !f.d.Local["clowdr.local/shop/server:v4"] {
		t.Fatal("current or previous image removed")
	}

	// A failed release loses its image but keeps its status.
	f.sick["clowdr.local/shop/server:bad"] = true
	bad, _ := f.deploy(t, "bad", serverSpec())
	if _, err := f.deploy(t, "v5", serverSpec()); err != nil {
		t.Fatal(err)
	}
	r, _ := f.st.Release("shop", "server", bad.ID)
	if r.Status != store.StatusFailed || !r.ImageRemoved || f.d.Local["clowdr.local/shop/server:bad"] {
		t.Fatalf("failed release after gc: %+v", r)
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
	// Another project's image, even when it is on the host.
	f.d.Local["clowdr.local/blog/server:v1"] = true
	if _, err := f.e.Deploy(context.Background(), Request{Project: "shop", Spec: serverSpec(), Image: "clowdr.local/blog/server:v1"}, nil); err == nil || !strings.Contains(err.Error(), "another project") {
		t.Fatalf("foreign image: %v", err)
	}
}

func TestCheckImage(t *testing.T) {
	for ref, ok := range map[string]bool{
		"nginx:alpine":             true,
		"ghcr.io/example/app:1.2":  true,
		"clowdr.local/shop/web:v1": true,
		"clowdr.local/shop/web@sha256:" + strings.Repeat("a", 64): true,
		"clowdr.local/blog/web:v1":                                false,
		"CLOWDR.LOCAL/blog/web:v1":                                false,
		"clowdr.local/shopx/web:v1":                               false, // a project named like a prefix of another
		"clowdr.local/shop":                                       false,
		"64d5d947aea7":                                            false, // image ID, or a prefix of one
		"64d5d9":                                                  false,
		"a":                                                       false,
		"sha256:" + strings.Repeat("a", 64):                       false,
		"cafe:latest":                                             true, // a name with a tag, not an ID
	} {
		if err := CheckImage("shop", ref); (err == nil) != ok {
			t.Errorf("CheckImage(shop, %q) = %v, want ok=%v", ref, err, ok)
		}
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
	if got := f.d.RunningWith("shop-server-r1"); len(got) != 1 {
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
	c := f.d.ByName[r.Containers[0]]
	if !strings.Contains(strings.Join(c.Config.Env, " "), "REDIS_URL=redis://redis:6379") {
		t.Fatal(c.Config.Env)
	}
	if rc := f.d.ByName["shop-redis"]; rc == nil || !rc.Running || rc.Config.Aliases[0] != "redis" {
		t.Fatal("redis addon not running")
	}
	ctx := context.Background()
	if err := f.e.Stop(ctx, "shop", "server"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.conf(t), "127.0.0.1:1 down") || len(f.d.RunningWith("shop-server")) != 0 {
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
	if f.conf(t) != "" || len(f.d.RunningWith("shop-server")) != 0 {
		t.Fatal("not removed")
	}
}

func TestScaleAndExec(t *testing.T) {
	f := newFixture(t)
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	nop := func(string, ...any) {}
	r, err := f.e.Scale(ctx, "shop", "server", 3, "test", nop)
	f.e.WaitDrains()
	if err != nil || len(r.Containers) != 3 || r.Image != "clowdr.local/shop/server:v1" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, _, err := f.e.Exec(ctx, "shop", "server", 3, []string{"true"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.e.Exec(ctx, "shop", "server", 4, []string{"true"}); err == nil {
		t.Fatal("replica 4 of 3")
	}
	// Does not fit: 1 GB host, 100m each.
	f.d.MemTotal = 1 << 30
	if _, err := f.e.Scale(ctx, "shop", "server", 12, "test", nop); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("want does not fit, got %v", err)
	}
	if _, err := f.e.Scale(ctx, "shop", "server", 0, "test", nop); err != nil {
		t.Fatal(err)
	}
	if svc, _ := f.st.Service("shop", "server"); !svc.Stopped {
		t.Fatal("scale 0 must stop")
	}
}

func TestExposeToNeedsAcceptFrom(t *testing.T) {
	f := newFixture(t)
	if err := f.st.PutProject(store.Project{Name: "blog"}); err != nil {
		t.Fatal(err)
	}
	deployBlog := func(accept ...string) {
		t.Helper()
		ref := "clowdr.local/blog/web:v1"
		f.d.Local[ref] = true
		web := spec.Service{Name: "web", AcceptFrom: accept, Drain: time.Millisecond,
			Health: spec.Health{Interval: time.Millisecond, Successes: 1}}
		web.Defaults()
		if _, err := f.e.Deploy(context.Background(), Request{Project: "blog", Spec: web, Image: ref, Author: "test"}, nil); err != nil {
			t.Fatal(err)
		}
		f.e.WaitDrains()
	}
	s := serverSpec()
	s.ExposeTo = []string{"blog"}

	// Without blog's consent shop cannot join blog's network.
	if _, err := f.deploy(t, "v1", s); err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("expose_to without accept_from: %v", err)
	}
	deployBlog("shop")
	rel, err := f.deploy(t, "v2", s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.d.Networks["clowdr-blog"], rel.Containers[0]) {
		t.Fatalf("shop not in blog's network: %v", f.d.Networks)
	}
	// Withdrawing consent disconnects the running container.
	deployBlog()
	if slices.Contains(f.d.Networks["clowdr-blog"], rel.Containers[0]) {
		t.Fatalf("shop still in blog's network: %v", f.d.Networks)
	}
}
