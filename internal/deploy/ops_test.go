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

	"github.com/veksa/clowdr/internal/deploy/fake"
	"github.com/veksa/clowdr/internal/ingress"
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
	// <project>-<addon> must not reach other containers whose names happen to match.
	if _, _, err := f.e.AddonExec(ctx, "shop", "server-r1-1", []string{"env"}); err == nil {
		t.Fatal("addon exec reached a service container")
	}
	if err := f.e.RemoveAddon(ctx, "shop", "server-r1-1", false); err == nil || f.d.ByName["shop-server-r1-1"] == nil {
		t.Fatalf("addon remove reached a service container: %v", err)
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
	// A volume of that name that clowdr did not make for shop stays.
	f.d.Vols["shop_redis-data"] = map[string]string{"com.docker.compose.project": "shop"}
	if err := f.e.RemoveAddon(ctx, "shop", "redis", true); err != nil || f.d.Vols["shop_redis-data"] == nil {
		t.Fatalf("foreign volume removed: %v", err)
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

// liveNginx stands for `docker exec clowdr-nginx nginx ...`: it fails when the container
// does not run, the way exec into a missing container does.
type liveNginx struct {
	d       *fake.Docker
	tests   int
	reloads int
	testErr error
}

func (n *liveNginx) running() error {
	if c := n.d.ByName[NginxContainer]; c == nil || !c.Running {
		return errors.New("no such container: " + NginxContainer)
	}
	return nil
}

func (n *liveNginx) Test(context.Context) (string, error) {
	if err := n.running(); err != nil {
		return "", err
	}
	n.tests++
	return "", n.testErr
}

func (n *liveNginx) Reload(context.Context) error {
	if err := n.running(); err != nil {
		return err
	}
	n.reloads++
	return nil
}

func TestStartNginx(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*fixture, *liveNginx) {
		f := newFixture(t)
		ng := &liveNginx{d: f.d}
		f.e.Ingress.Nginx = ng
		return f, ng
	}
	read := func(f *fixture, name string) string {
		b, _ := os.ReadFile(filepath.Join(f.dir, name))
		return string(b)
	}
	// started puts the host where a previous clowdrd left it: nginx running the configured
	// image on the config that version wrote.
	started := func(t *testing.T, f *fixture, ng *liveNginx, mainConf string) {
		t.Helper()
		if err := f.e.StartNginx(ctx); err != nil {
			t.Fatal(err)
		}
		if mainConf != "" {
			os.WriteFile(filepath.Join(f.dir, "nginx.conf"), []byte(mainConf), 0o644)
		}
		ng.tests, ng.reloads = 0, 0
	}

	t.Run("first start", func(t *testing.T) {
		f, ng := setup(t)
		if err := f.e.StartNginx(ctx); err != nil {
			t.Fatal(err)
		}
		if c := f.d.ByName[NginxContainer]; c == nil || !c.Running {
			t.Fatal("nginx not started")
		}
		if read(f, "nginx.conf") != ingress.RenderMain(f.e.IngressOptions()) || read(f, "proxy_defaults.conf") != ingress.ProxyDefaultsConf {
			t.Fatal("main config not written")
		}
		// The fresh container read the files when it started: nothing to test or reload
		// (and liveNginx fails any exec that runs before the container exists).
		if ng.tests != 0 || ng.reloads != 0 {
			t.Fatalf("tests=%d reloads=%d", ng.tests, ng.reloads)
		}
	})

	t.Run("upgrade with nginx running", func(t *testing.T) {
		f, ng := setup(t)
		started(t, f, ng, "# written by the previous version\n")
		c := f.d.ByName[NginxContainer]
		if err := f.e.StartNginx(ctx); err != nil {
			t.Fatal(err)
		}
		if read(f, "nginx.conf") != ingress.RenderMain(f.e.IngressOptions()) {
			t.Fatal("new nginx.conf not written")
		}
		if ng.tests != 1 || ng.reloads != 1 {
			t.Fatalf("a changed nginx.conf needs one checked reload: tests=%d reloads=%d", ng.tests, ng.reloads)
		}
		if f.d.ByName[NginxContainer] != c {
			t.Fatal("nginx recreated instead of reloaded")
		}
	})

	t.Run("unchanged config", func(t *testing.T) {
		f, ng := setup(t)
		started(t, f, ng, "")
		if err := f.e.StartNginx(ctx); err != nil {
			t.Fatal(err)
		}
		if ng.tests != 0 || ng.reloads != 0 {
			t.Fatalf("nothing changed, nothing to reload: tests=%d reloads=%d", ng.tests, ng.reloads)
		}
	})

	t.Run("rejected config", func(t *testing.T) {
		f, ng := setup(t)
		old := "# written by the previous version\n"
		started(t, f, ng, old)
		ng.testErr = errors.New("nginx -t exited with 1")
		if err := f.e.StartNginx(ctx); err != nil {
			t.Fatalf("the daemon must still start on the previous config: %v", err)
		}
		if read(f, "nginx.conf") != old {
			t.Fatal("previous nginx.conf not restored")
		}
		if ng.reloads != 0 || !f.d.ByName[NginxContainer].Running {
			t.Fatalf("nginx must keep running unreloaded: reloads=%d", ng.reloads)
		}
	})

	t.Run("stopped nginx starts on the new config", func(t *testing.T) {
		f, ng := setup(t)
		started(t, f, ng, "# written by the previous version\n")
		f.d.ByName[NginxContainer].Running = false
		if err := f.e.StartNginx(ctx); err != nil {
			t.Fatal(err)
		}
		if read(f, "nginx.conf") != ingress.RenderMain(f.e.IngressOptions()) || !f.d.ByName[NginxContainer].Running {
			t.Fatal("nginx not started on the new config")
		}
		// It read the new file when it started: no reload needed.
		if ng.reloads != 0 {
			t.Fatalf("reloads=%d", ng.reloads)
		}
	})
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

func TestNginxReloadWaitsForNewWorkers(t *testing.T) {
	f := newFixture(t)
	n := NginxExec{Docker: f.d}
	ps := 0
	f.d.ExecFn = func(_ string, cmd []string) (string, int, error) {
		if cmd[0] != "ps" {
			return "", 0, nil
		}
		ps++
		switch {
		case ps == 1: // before the reload
			return "PID   COMMAND\n    7 nginx: master process nginx\n   91 nginx: worker process\n", 0, nil
		case ps < 4: // the master has not started the new workers yet
			return "    7 nginx: master process nginx\n   91 nginx: worker process\n", 0, nil
		default:
			return "    7 nginx: master process nginx\n   91 nginx: worker process is shutting down\n  120 nginx: worker process\n", 0, nil
		}
	}
	if err := n.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps != 4 {
		t.Fatalf("ps ran %d times, want 4", ps)
	}
}

func TestRemoveProject(t *testing.T) {
	f := newFixture(t)
	plat := f.e.Platform()
	redis := plat.Addons["redis"]
	redis.Volume = "/data"
	plat.Addons["redis"] = redis
	ctx := context.Background()
	s := serverSpec()
	s.Uses = []string{"redis"}
	s.Volumes = []spec.Volume{{Name: "uploads", Target: "/app/uploads"}}
	if _, err := f.deploy(t, "v1", s); err != nil {
		t.Fatal(err)
	}

	if err := f.e.RemoveProject(ctx, "shop", false); err != nil {
		t.Fatal(err)
	}
	if cs, _ := f.d.Containers(ctx, "clowdr.project=shop"); len(cs) != 0 {
		t.Fatalf("containers left, the addon included: %+v", cs)
	}
	if slices.Contains(f.d.Networks[NetworkName("shop")], NginxContainer) {
		t.Error("nginx still in the project network")
	}
	left, err := f.e.LeftoverVolumes(ctx, "shop")
	if err != nil || !slices.Equal(left, []string{"shop_redis-data", "shop_uploads"}) {
		t.Fatalf("volumes are kept by default: %v %v", left, err)
	}

	if err := f.e.RemoveProject(ctx, "shop", true); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.e.LeftoverVolumes(ctx, "shop"); len(left) != 0 {
		t.Fatalf("volumes=true left %v", left)
	}
}
