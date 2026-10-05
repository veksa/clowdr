package deploy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

func nop(string, ...any) {}

func TestRestartKeepsTheImage(t *testing.T) {
	f := newFixture(t)
	r1, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f.e.Restart(context.Background(), "shop", "server", "test", nop)
	f.e.WaitDrains()
	if err != nil || r2.ID == r1.ID || r2.Image != r1.Image || r2.Version != "v1" {
		t.Fatalf("restart: %+v %v", r2, err)
	}
	if got := f.d.RunningWith("shop-server"); len(got) != 1 || got[0] != r2.Containers[0] {
		t.Fatalf("running %v", got)
	}
	if _, err := f.e.Restart(context.Background(), "shop", "nope", "test", nop); err == nil {
		t.Fatal("restart of an unknown service")
	}
}

func TestGCRemovesOrphansAndStrayImages(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"v1", "v2"} {
		if _, err := f.deploy(t, v, serverSpec()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	// A container left by a crashed clowdrd, an image pushed long ago and never deployed,
	// and one pushed a moment ago (a deploy is about to follow).
	f.d.ContainerCreate(ctx, runtime.ContainerConfig{Name: "shop-server-r99-1", Image: "x",
		Labels: map[string]string{runtime.LabelManaged: "1", runtime.LabelRole: "service"}})
	f.d.AddImage("clowdr.local/shop/server:stray")
	f.d.AddImage("clowdr.local/shop/server:fresh")
	f.e.Pushed("clowdr.local/shop/server:fresh")
	f.d.AddImage("nginx:alpine")

	res, err := f.e.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Containers, []string{"shop-server-r99-1"}) {
		t.Fatalf("containers %v", res.Containers)
	}
	if !slices.Equal(res.Images, []string{"clowdr.local/shop/server:stray"}) {
		t.Fatalf("images %v", res.Images)
	}
	for _, keep := range []string{"v1", "v2", "fresh"} {
		if !f.d.Local["clowdr.local/shop/server:"+keep] {
			t.Errorf("GC removed %s", keep)
		}
	}
	if !f.d.Local["nginx:alpine"] {
		t.Error("GC removed an image clowdr does not own")
	}
}

func TestVolumesAndAddons(t *testing.T) {
	f := newFixture(t)
	plat := f.e.Platform()
	redis := plat.Addons["redis"]
	redis.Volume, redis.Memory = "/data", 64<<20
	plat.Addons["redis"] = redis
	ctx := context.Background()

	s := serverSpec()
	s.Uses = []string{"redis"}
	s.Volumes = []spec.Volume{{Name: "uploads", Target: "/app/uploads"}}
	if _, err := f.deploy(t, "v1", s); err != nil {
		t.Fatal(err)
	}
	rc := f.d.ByName["shop-redis"]
	if rc == nil || !slices.Contains(rc.Config.Cmd, "--maxmemory") || rc.Config.Mounts[0].Target != "/data" {
		t.Fatalf("redis: %+v", rc)
	}

	vols, err := f.e.Volumes(ctx, "shop")
	if err != nil || len(vols) != 2 {
		t.Fatalf("volumes %+v %v", vols, err)
	}
	used := map[string]string{}
	for _, v := range vols {
		used[v.Name] = strings.Join(v.UsedBy, ",")
	}
	if used["uploads"] != "server" || used["redis-data"] != "addon redis" {
		t.Fatalf("used by %v", used)
	}
	if err := f.e.RemoveVolume(ctx, "shop", "uploads"); err == nil || !strings.Contains(err.Error(), "used by server") {
		t.Fatalf("removed a volume in use: %v", err)
	}
	if err := f.e.RemoveVolume(ctx, "shop", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown volume: %v", err)
	}

	addons, err := f.e.Addons(ctx, "shop")
	if err != nil || len(addons) != 1 || addons[0].Name != "redis" || addons[0].Memory != 64<<20 || addons[0].State != "running" {
		t.Fatalf("addons %+v %v", addons, err)
	}
	f.d.ExecFn = func(c string, cmd []string) (string, int, error) { return "PONG\n", 0, nil }
	if out, code, err := f.e.AddonExec(ctx, "shop", "redis", []string{"redis-cli", "ping"}); err != nil || code != 0 || out != "PONG\n" {
		t.Fatalf("addon exec %q %d %v", out, code, err)
	}
	if _, _, err := f.e.AddonExec(ctx, "shop", "postgres", []string{"psql"}); err == nil {
		t.Fatal("exec in a missing addon")
	}
	if _, _, err := f.e.AddonExec(ctx, "shop", "redis", nil); err == nil {
		t.Fatal("exec without a command")
	}
	if err := f.e.RemoveAddon(ctx, "shop", "redis", true); err == nil || !strings.Contains(err.Error(), "uses redis") {
		t.Fatalf("removed an addon in use: %v", err)
	}

	// Once the service is gone, both can go; volumes only when asked.
	if err := f.e.Remove(ctx, "shop", "server"); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RemoveVolume(ctx, "shop", "uploads"); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RemoveAddon(ctx, "shop", "redis", false); err != nil {
		t.Fatal(err)
	}
	if f.d.ByName["shop-redis"] != nil || f.d.Vols["shop_redis-data"] == nil {
		t.Fatal("addon container should be gone, its data kept")
	}
	if err := f.e.RemoveAddon(ctx, "shop", "redis", true); err != nil || f.d.Vols["shop_redis-data"] != nil {
		t.Fatalf("addon data not removed: %v", err)
	}
}

func TestEnsureNginx(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	image := f.e.Platform().Nginx.Image

	// First start: pull, create, start, connect to every project network.
	if err := f.e.EnsureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	c := f.d.ByName[NginxContainer]
	if c == nil || !c.Running || c.Config.Image != image || !f.d.Local[image] {
		t.Fatalf("nginx: %+v", c)
	}
	if !slices.Contains(f.d.Networks[NetworkName("shop")], NginxContainer) {
		t.Fatal("nginx is not in the project network")
	}

	// Stopped: started again, not recreated.
	c.Running = false
	if err := f.e.EnsureNginx(ctx); err != nil || !f.d.ByName[NginxContainer].Running || f.d.ByName[NginxContainer] != c {
		t.Fatal("nginx not restarted", err)
	}

	// The image changed in platform.yml: recreated.
	f.e.Platform().Nginx.Image = "nginx:1.29-alpine"
	if err := f.e.EnsureNginx(ctx); err != nil {
		t.Fatal(err)
	}
	if c := f.d.ByName[NginxContainer]; c.Config.Image != "nginx:1.29-alpine" || !c.Running {
		t.Fatalf("not recreated: %+v", c.Config)
	}
}

func TestNginxExec(t *testing.T) {
	f := newFixture(t)
	n := NginxExec{Docker: f.d}
	ctx := context.Background()
	if _, err := n.Test(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.d.Execs, "\n"), NginxContainer+": nginx -s reload") {
		t.Fatal(f.d.Execs)
	}
	f.d.ExecFn = func(string, []string) (string, int, error) { return "unknown directive \"x\"", 1, nil }
	if out, err := n.Test(ctx); err == nil || !strings.Contains(out, "unknown directive") {
		t.Fatalf("nginx -t failure: %q %v", out, err)
	}
	if err := n.Reload(ctx); err == nil || !strings.Contains(err.Error(), "unknown directive") {
		t.Fatalf("reload failure: %v", err)
	}
	f.d.ExecFn = func(string, []string) (string, int, error) { return "", -1, errors.New("no such container") }
	if _, err := n.Test(ctx); err == nil {
		t.Fatal("exec error")
	}
	if err := n.Reload(ctx); err == nil {
		t.Fatal("exec error")
	}
}

func TestRenderAllDropsDeletedProjects(t *testing.T) {
	f := newFixture(t)
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(f.dir, "conf.d", "gone.conf")
	os.WriteFile(stale, []byte("server {}"), 0o644)
	if err := f.e.RenderAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("config of a deleted project is still there")
	}
	if !strings.Contains(f.conf(t), "api.example.com") {
		t.Fatal("project config missing")
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "nginx.conf")); err != nil || len(b) == 0 {
		t.Fatal("main config missing", err)
	}
}

func TestCertDomains(t *testing.T) {
	f := newFixture(t)
	if got := f.e.CertDomains(); len(got) != 0 {
		t.Fatal(got)
	}
	s := serverSpec()
	s.TLS = spec.TLSDirect
	s.Domains = []string{"stream.example.com"}
	if _, err := f.deploy(t, "v1", s); err != nil {
		t.Fatal(err)
	}
	f.e.Platform().Domain = "example.com"
	if got := f.e.CertDomains(); !slices.Equal(got, []string{"deploy.example.com", "stream.example.com"}) {
		t.Fatal(got)
	}
}

func TestHTTPProber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	p := httpProber{c: &http.Client{Timeout: time.Second}}
	ctx := context.Background()
	if err := p.Probe(ctx, srv.URL+"/health"); err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(ctx, srv.URL+"/other"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatal(err)
	}
	if err := p.Probe(ctx, "http://127.0.0.1:1/health"); err == nil {
		t.Fatal("refused connection")
	}
}
