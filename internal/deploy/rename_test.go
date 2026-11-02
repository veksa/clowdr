package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

func (f *fixture) confOf(project string) string {
	b, err := os.ReadFile(filepath.Join(f.dir, "conf.d", project+".conf"))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestRenameProject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	rel, err := f.deploy(t, "v2", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.st.PutToken(store.Token{ID: "t1", Hash: "h", Project: "shop"})
	f.st.Own("shop", "sha256:abc")
	var moved [][2]string
	f.e.MoveLogs = func(old, name string) { moved = append(moved, [2]string{old, name}) }

	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()

	// State moved, in one piece.
	if _, err := f.st.Project("shop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old project is still there: %v", err)
	}
	p, err := f.st.Project("store")
	if err != nil || p.RenamedFrom != "" || !slices.Contains(p.Domains, "example.com") {
		t.Fatalf("project %+v, %v", p, err)
	}
	svc, err := f.st.Service("store", "server")
	if err != nil || svc.Project != "store" || svc.Current != rel.ID {
		t.Fatalf("service %+v, %v", svc, err)
	}
	if vs, _ := f.st.Services("shop"); len(vs) != 0 {
		t.Fatalf("old services left: %+v", vs)
	}
	got, err := f.st.Release("store", "server", rel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "store" || got.Image != "clowdr.local/store/server:v2" {
		t.Fatalf("release %+v", got)
	}
	want := []string{containerName("store", "server", rel.ID, 1)}
	if !slices.Equal(got.Containers, want) {
		t.Fatalf("containers %v, want %v", got.Containers, want)
	}
	if rs, _ := f.st.Releases("store", "server"); len(rs) != 2 {
		t.Fatalf("history lost: %+v", rs)
	}
	if tok, err := f.st.Token("t1"); err != nil || tok.Project != "store" {
		t.Fatalf("token %+v, %v", tok, err)
	}
	if ok, _ := f.st.Owns("store", "sha256:abc"); !ok {
		t.Error("blob ownership did not move")
	}

	// Docker: the new container is live under the new labels, nothing old remains.
	if cs, _ := f.d.Containers(ctx, "clowdr.project=shop"); len(cs) != 0 {
		t.Fatalf("old containers left: %+v", cs)
	}
	c := f.d.ByName[want[0]]
	if c == nil || !c.Running {
		t.Fatalf("new container %+v", c)
	}
	if c.Config.Labels[runtime.LabelProject] != "store" || c.Config.Network != NetworkName("store") {
		t.Fatalf("new container config %+v", c.Config)
	}
	if !slices.Contains(c.Config.Env, "CLOWDR_PROJECT=store") {
		t.Fatalf("env %v", c.Config.Env)
	}
	if !slices.Contains(f.d.Networks[NetworkName("store")], NginxContainer) {
		t.Error("nginx is not in the new network")
	}
	if _, ok := f.d.Networks[NetworkName("shop")]; ok {
		t.Error("old network is still there")
	}
	if f.d.Local["clowdr.local/shop/server:v2"] || !f.d.Local["clowdr.local/store/server:v2"] {
		t.Fatalf("images %v", f.d.Local)
	}

	// nginx: one config, pointing at the new container.
	if f.confOf("shop") != "" {
		t.Error("old nginx config is still there")
	}
	if conf := f.confOf("store"); !strings.Contains(conf, want[0]) || !strings.Contains(conf, "api.example.com") {
		t.Fatalf("new nginx config:\n%s", conf)
	}

	// Logs moved at the switch and again after the drain.
	if !slices.Equal(moved, [][2]string{{"shop", "store"}, {"shop", "store"}}) {
		t.Fatalf("logs moved %v", moved)
	}

	// The project keeps working: both a new deploy and a rollback to a release from before.
	s := serverSpec()
	if _, err := f.e.Deploy(ctx, Request{Project: "store", Spec: s, Image: "clowdr.local/store/server:v2", Version: "v2", Author: "test"}, nil); err != nil {
		t.Fatalf("deploy under the new name: %v", err)
	}
	if _, err := f.e.Rollback(ctx, "store", "server", 0, "test", func(string, ...any) {}); err != nil {
		t.Fatalf("rollback under the new name: %v", err)
	}
	f.e.WaitDrains()
}

func TestRenameProjectRefuses(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, f *fixture)
		to      string
		want    string
	}{
		{"invalid name", nil, "Not Valid", "invalid project name"},
		{"reserved name", nil, "clowdr", "reserved"},
		{"same name", nil, "shop", "already called"},
		{"name taken", func(t *testing.T, f *fixture) { f.st.PutProject(store.Project{Name: "store"}) }, "store", "already exists"},
		{"prefix clash", func(t *testing.T, f *fixture) { f.st.PutProject(store.Project{Name: "store-eu"}) }, "store", "clashes"},
		{"seeded", func(t *testing.T, f *fixture) {
			p, _ := f.st.Project("shop")
			p.Seeded = true
			f.st.PutProject(p)
		}, "store", "platform.yml"},
		{"volume not named after the project", func(t *testing.T, f *fixture) {
			f.d.VolumeEnsure(context.Background(), "odd", map[string]string{runtime.LabelProject: "shop"})
		}, "store", "not named after it"},
		{"addon", func(t *testing.T, f *fixture) {
			s := serverSpec()
			s.Uses = []string{"redis"}
			if _, err := f.deploy(t, "v1", s); err != nil {
				t.Fatal(err)
			}
		}, "store", "addons"},
		{"stopped service", func(t *testing.T, f *fixture) {
			if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
				t.Fatal(err)
			}
			if err := f.e.Stop(context.Background(), "shop", "server"); err != nil {
				t.Fatal(err)
			}
		}, "store", "stopped"},
		{"volumes of a removed project under the new name", func(t *testing.T, f *fixture) {
			f.d.VolumeEnsure(context.Background(), "store_data", map[string]string{runtime.LabelProject: "store"})
		}, "store", "removed project"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			if c.prepare != nil {
				c.prepare(t, f)
			}
			err := f.e.RenameProject(context.Background(), "shop", c.to, "test", nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if _, err := f.st.Project("shop"); err != nil {
				t.Fatalf("the project must stay: %v", err)
			}
		})
	}
}

func TestRenameProjectFailedHealthLeavesItAsItWas(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rel, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	conf := f.confOf("shop")
	oldName := rel.Containers[0]
	f.sick[containerName("store", "server", rel.ID, 1)] = true
	f.sick["clowdr.local/store/server:v1"] = true

	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); !errors.Is(err, ErrHealth) {
		t.Fatalf("got %v, want a health failure", err)
	}
	if _, err := f.st.Project("store"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new project exists: %v", err)
	}
	if svc, err := f.st.Service("shop", "server"); err != nil || svc.Current != rel.ID {
		t.Fatalf("service %+v, %v", svc, err)
	}
	if got, _ := f.st.Release("shop", "server", rel.ID); got.Image != "clowdr.local/shop/server:v1" || !slices.Equal(got.Containers, []string{oldName}) {
		t.Fatalf("release %+v", got)
	}
	if c := f.d.ByName[oldName]; c == nil || !c.Running {
		t.Fatalf("old container %+v", c)
	}
	if cs, _ := f.d.Containers(ctx, "clowdr.project=store"); len(cs) != 0 {
		t.Fatalf("new containers left: %+v", cs)
	}
	if f.confOf("shop") != conf || f.confOf("store") != "" {
		t.Fatal("nginx config changed")
	}
	if f.d.Local["clowdr.local/store/server:v1"] {
		t.Error("new image tag left")
	}
	if _, ok := f.d.Networks[NetworkName("store")]; ok {
		t.Error("new network left")
	}
	// Nothing in the way of a second try.
	delete(f.sick, containerName("store", "server", rel.ID, 1))
	delete(f.sick, "clowdr.local/store/server:v1")
	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err != nil {
		t.Fatalf("second try: %v", err)
	}
	f.e.WaitDrains()
}

func TestRenameProjectNginxRejectsConfig(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rel, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	conf := f.confOf("shop")
	f.nginx.TestErr = errors.New("bad config")

	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err == nil {
		t.Fatal("rename went through a rejected config")
	}
	f.nginx.TestErr = nil
	p, err := f.st.Project("shop")
	if err != nil || p.RenamedFrom != "" {
		t.Fatalf("project %+v, %v", p, err)
	}
	if got, _ := f.st.Release("shop", "server", rel.ID); got.Image != "clowdr.local/shop/server:v1" || !slices.Equal(got.Containers, rel.Containers) {
		t.Fatalf("release %+v", got)
	}
	if f.confOf("shop") != conf || f.confOf("store") != "" {
		t.Fatal("nginx config did not come back")
	}
	if cs, _ := f.d.Containers(ctx, "clowdr.project=store"); len(cs) != 0 {
		t.Fatalf("new containers left: %+v", cs)
	}
	if c := f.d.ByName[rel.Containers[0]]; c == nil || !c.Running {
		t.Fatalf("old container %+v", c)
	}
}

// A container exposed to the project (a shared redis) joins the new network without a restart
// and leaves the old one, and both specs carry the new name afterwards.
func TestRenameProjectExposedContainer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := serverSpec()
	s.AcceptFrom = []string{"shared"}
	if _, err := f.deploy(t, "v1", s); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutProject(store.Project{Name: "shared"}); err != nil {
		t.Fatal(err)
	}
	redis := spec.Service{Name: "redis", ExposeTo: []string{"shop"}, Switch: spec.SwitchStopFirst}
	redis.Defaults()
	f.d.Local["redis:7"] = true
	rr, err := f.e.Deploy(ctx, Request{Project: "shared", Spec: redis, Image: "redis:7", Author: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	redisC := rr.Containers[0]
	if !slices.Contains(f.d.Networks[NetworkName("shop")], redisC) {
		t.Fatal("redis is not in the project network")
	}

	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()

	if !slices.Contains(f.d.Networks[NetworkName("store")], redisC) {
		t.Error("redis did not join the new network")
	}
	if slices.Contains(f.d.Networks[NetworkName("shop")], redisC) {
		t.Error("redis is still in the old network")
	}
	if c := f.d.ByName[redisC]; c == nil || !c.Running {
		t.Fatalf("redis was touched: %+v", c)
	}
	v, err := f.st.Service("shared", "redis")
	if err != nil || !slices.Equal(v.Spec.ExposeTo, []string{"store"}) {
		t.Fatalf("redis spec %+v, %v", v.Spec.ExposeTo, err)
	}
	if got, _ := f.st.Release("shared", "redis", rr.ID); !slices.Equal(got.Spec.ExposeTo, []string{"store"}) || !slices.Equal(got.Containers, rr.Containers) {
		t.Fatalf("redis release %+v", got)
	}
	// Its next deploy keeps working, with the rewritten spec.
	redis.ExposeTo = []string{"store"}
	if _, err := f.e.Deploy(ctx, Request{Project: "shared", Spec: redis, Image: "redis:7", Author: "test"}, nil); err != nil {
		t.Fatalf("redeploy of the exposed service: %v", err)
	}
	f.e.WaitDrains()
}

// A service that exposes itself to another project keeps that link under the new name, and the
// other project's accept_from follows.
func TestRenameProjectExposingService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.st.PutProject(store.Project{Name: "web"})
	web := spec.Service{Name: "web", AcceptFrom: []string{"shop"}}
	web.Defaults()
	f.d.Local["web:1"] = true
	if _, err := f.e.Deploy(ctx, Request{Project: "web", Spec: web, Image: "web:1", Author: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	s := serverSpec()
	s.ExposeTo = []string{"web"}
	rel, err := f.deploy(t, "v1", s)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()

	c := containerName("store", "server", rel.ID, 1)
	if !slices.Contains(f.d.Networks[NetworkName("web")], c) {
		t.Errorf("%s is not in the other project's network: %v", c, f.d.Networks[NetworkName("web")])
	}
	v, err := f.st.Service("web", "web")
	if err != nil || !slices.Equal(v.Spec.AcceptFrom, []string{"store"}) {
		t.Fatalf("accept_from %v, %v", v.Spec.AcceptFrom, err)
	}
}

func TestRenameProjectStopFirstService(t *testing.T) {
	f := newFixture(t)
	s := serverSpec()
	s.Switch = spec.SwitchStopFirst
	rel, err := f.deploy(t, "v1", s)
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	err = f.e.RenameProject(context.Background(), "shop", "store", "test", func(format string, a ...any) { log = append(log, format) })
	if err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()
	if !slices.ContainsFunc(log, func(l string) bool { return strings.HasPrefix(l, "stop-first") }) {
		t.Errorf("no stop-first step: %v", log)
	}
	if c := f.d.ByName[containerName("store", "server", rel.ID, 1)]; c == nil || !c.Running {
		t.Fatalf("new container %+v", c)
	}
}

// A volume nothing mounts is copied while the service keeps running; one a service mounts is copied
// with the service down, so nothing it writes is lost. The originals go only after the switch.
func TestRenameProjectCopiesVolumes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	labels := map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: "shop"}
	mounted := serverSpec()
	mounted.Name, mounted.Domains = "files", []string{"files.example.com"}
	mounted.Volumes = []spec.Volume{{Name: "uploads", Target: "/uploads"}}
	filesRel, err := f.deploy(t, "v1", mounted)
	if err != nil {
		t.Fatal(err)
	}
	apiRel, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.d.VolumeEnsure(ctx, "shop_logs", labels)

	var copies []string
	runningDuringCopy := map[string]bool{}
	var oldVolumesDuringCopy int
	f.d.ExecFn = func(c string, cmd []string) (string, int, error) {
		copies = append(copies, c)
		runningDuringCopy[filesRel.Containers[0]] = f.d.ByName[filesRel.Containers[0]].Running
		runningDuringCopy[apiRel.Containers[0]] = f.d.ByName[apiRel.Containers[0]].Running
		vols, _ := f.d.Volumes(ctx, runtime.LabelProject+"=shop")
		oldVolumesDuringCopy = len(vols)
		return "3 files, 12K\n", 0, nil
	}

	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()

	if len(copies) != 2 {
		t.Fatalf("copies %v", copies)
	}
	if oldVolumesDuringCopy != 2 {
		t.Errorf("old volumes during the copy: %d", oldVolumesDuringCopy)
	}
	if runningDuringCopy[filesRel.Containers[0]] {
		t.Error("a service was running while its volume was copied")
	}
	if !runningDuringCopy[apiRel.Containers[0]] {
		t.Error("a service without volumes was stopped for the copy")
	}
	if vols, _ := f.d.Volumes(ctx, runtime.LabelProject+"=shop"); len(vols) != 0 {
		t.Fatalf("old volumes left: %+v", vols)
	}
	vols, _ := f.d.Volumes(ctx, runtime.LabelProject+"=store")
	if len(vols) != 2 || vols[0].Name != "store_logs" || vols[1].Name != "store_uploads" {
		t.Fatalf("new volumes %+v", vols)
	}
	for c := range f.d.ByName {
		if strings.HasPrefix(c, "clowdr-rename-") {
			t.Errorf("helper container left: %s", c)
		}
	}
	c := f.d.ByName[containerName("store", "files", filesRel.ID, 1)]
	if c == nil || !c.Running {
		t.Fatalf("new container %+v", c)
	}
	if len(c.Config.Mounts) != 1 || c.Config.Mounts[0].Source != "store_uploads" {
		t.Fatalf("mounts %+v", c.Config.Mounts)
	}
	// A later deploy finds the copy as its own volume.
	if _, err := f.e.Deploy(ctx, Request{Project: "store", Spec: mounted, Image: "clowdr.local/store/server:v1", Author: "test"}, nil); err != nil {
		t.Fatalf("deploy with the copied volume: %v", err)
	}
	f.e.WaitDrains()
}

func TestRenameProjectFailedCopyLeavesItAsItWas(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := serverSpec()
	s.Volumes = []spec.Volume{{Name: "uploads", Target: "/uploads"}}
	rel, err := f.deploy(t, "v1", s)
	if err != nil {
		t.Fatal(err)
	}
	f.d.ExecFn = func(string, []string) (string, int, error) { return "not enough disk\n", 1, nil }

	err = f.e.RenameProject(ctx, "shop", "store", "test", nil)
	if err == nil || !strings.Contains(err.Error(), "not enough disk") {
		t.Fatalf("got %v", err)
	}
	if _, err := f.st.Project("shop"); err != nil {
		t.Fatal(err)
	}
	if c := f.d.ByName[rel.Containers[0]]; c == nil || !c.Running {
		t.Fatalf("the service did not come back: %+v", c)
	}
	if f.d.Vols["shop_uploads"] == nil {
		t.Error("the original volume is gone")
	}
	if f.d.Vols["store_uploads"] != nil {
		t.Error("the partial copy is left")
	}
	for c := range f.d.ByName {
		if strings.HasPrefix(c, "clowdr-rename-") {
			t.Errorf("helper container left: %s", c)
		}
	}
}

// A partial copy left by a clowdrd that died mid-rename does not block the next try, but a volume
// of someone else under the new name does.
func TestRenameProjectInterruptedCopy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.d.VolumeEnsure(ctx, "shop_logs", map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: "shop"})
	f.d.VolumeEnsure(ctx, "store_logs", map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: "store", labelRenamedFrom: "shop"})
	f.d.VolumeEnsure(ctx, "other_logs", map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: "other", labelRenamedFrom: "elsewhere"})

	if err := f.e.RenameProject(ctx, "shop", "other", "test", nil); err == nil || !strings.Contains(err.Error(), "removed project") {
		t.Fatalf("a volume from another rename: %v", err)
	}
	if err := f.e.RenameProject(ctx, "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()
	if f.d.Vols["store_logs"] == nil || f.d.Vols["shop_logs"] != nil {
		t.Fatalf("volumes %v", f.d.Vols)
	}
}

// Recover finishes a rename clowdrd did not live to finish: traffic has moved, the old copy goes.
func TestRecoverFinishesRename(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rel, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	oldC := rel.Containers[0]
	newC := containerName("store", "server", rel.ID, 1)
	s := serverSpec()
	f.d.Local["clowdr.local/store/server:v1"] = true
	if err := f.e.connectNginx(ctx, "store"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.startContainer(ctx, "store", s, rel.ID, 1, newC, "clowdr.local/store/server:v1", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RenameProject("shop", "store", func(r *store.Release) {
		r.Image, r.Containers = "clowdr.local/store/server:v1", []string{newC}
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.e.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()

	if f.d.ByName[oldC] != nil {
		t.Error("old container is still there")
	}
	if c := f.d.ByName[newC]; c == nil || !c.Running {
		t.Fatalf("new container %+v", c)
	}
	if p, err := f.st.Project("store"); err != nil || p.RenamedFrom != "" {
		t.Fatalf("project %+v, %v", p, err)
	}
	if f.d.Local["clowdr.local/shop/server:v1"] {
		t.Error("old image tag left")
	}
}

// A deploy that waited for the rename fails on the old name instead of writing under it.
func TestDeployAfterRenameNeedsNewName(t *testing.T) {
	f := newFixture(t)
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RenameProject(context.Background(), "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()
	_, err := f.e.Deploy(context.Background(), Request{Project: "shop", Spec: serverSpec(), Image: "clowdr.local/shop/server:v1", Author: "test"}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("got %v", err)
	}
	if vs, _ := f.st.Services("shop"); len(vs) != 0 {
		t.Fatalf("written under the old name: %+v", vs)
	}
}
