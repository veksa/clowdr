package deploy

import (
	"context"
	"slices"
	"testing"

	"github.com/veksa/clowdr/internal/store"
)

// interrupted leaves a release the way a killed clowdrd does: recorded as deploying, its
// container started (and recorded only when record is set).
func (f *fixture) interrupted(t *testing.T, img string, record bool) store.Release {
	t.Helper()
	s := serverSpec()
	rel := store.Release{Project: "shop", Service: "server", Image: f.push(img), Version: img, Status: store.StatusDeploying, Spec: s}
	if err := f.st.CreateRelease(&rel); err != nil {
		t.Fatal(err)
	}
	name := containerName("shop", "server", rel.ID, 1)
	if _, err := f.e.startContainer(context.Background(), "shop", s, rel.ID, 1, name, rel.Image, nil); err != nil {
		t.Fatal(err)
	}
	if record {
		rel.Containers = []string{name}
		f.st.PutRelease(rel)
	}
	return rel
}

func (f *fixture) release(t *testing.T, id uint64) store.Release {
	t.Helper()
	r, err := f.st.Release("shop", "server", id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecoverFailsAnUnfinishedDeploy(t *testing.T) {
	f := newFixture(t)
	r1, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	r2 := f.interrupted(t, "v2", false)

	// While the deploy is in flight, GC keeps its container although the release does not
	// list it yet.
	if res, err := f.e.GC(context.Background()); err != nil || len(res.Containers) != 0 {
		t.Fatalf("gc during a deploy: %v %v", res, err)
	}

	if err := f.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()
	if r := f.release(t, r2.ID); r.Status != store.StatusFailed || r.Error == "" {
		t.Fatalf("r2: %+v", r)
	}
	if r := f.release(t, r1.ID); r.Status != store.StatusActive {
		t.Fatalf("r1: %+v", r)
	}
	if got := f.d.RunningWith("shop-server"); !slices.Equal(got, r1.Containers) {
		t.Fatalf("running %v, want %v", got, r1.Containers)
	}
	if _, ok := f.d.ByName[containerName("shop", "server", r2.ID, 1)]; ok {
		t.Fatal("container of the failed release is still there")
	}
	// The next deploy works.
	if _, err := f.deploy(t, "v3", serverSpec()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverFinishesASwitchedDeploy(t *testing.T) {
	f := newFixture(t)
	r1, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	// Killed right after the switch: state points at r2, r1 still runs, nothing drained.
	r2 := f.interrupted(t, "v2", true)
	f.st.UpdateService("shop", "server", func(v *store.Service) error {
		v.Previous, v.Current = v.Current, r2.ID
		return nil
	})

	if err := f.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()
	if r := f.release(t, r2.ID); r.Status != store.StatusActive {
		t.Fatalf("r2: %+v", r)
	}
	if r := f.release(t, r1.ID); r.Status != store.StatusSuperseded {
		t.Fatalf("r1: %+v", r)
	}
	if got := f.d.RunningWith("shop-server"); !slices.Equal(got, r2.Containers) {
		t.Fatalf("running %v, want %v", got, r2.Containers)
	}
	if _, ok := f.d.ByName[r1.Containers[0]]; ok {
		t.Fatal("previous release was not drained")
	}
}

func TestRecoverStartsTheCurrentRelease(t *testing.T) {
	f := newFixture(t)
	r1, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	// Killed in a stop-first deploy: the old containers are stopped, the new one is running
	// but was never switched to.
	f.d.ContainerStop(context.Background(), r1.Containers[0], 0)
	r2 := f.interrupted(t, "v2", true)

	if err := f.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.d.RunningWith("shop-server"); !slices.Equal(got, r1.Containers) {
		t.Fatalf("running %v, want %v", got, r1.Containers)
	}
	if r := f.release(t, r2.ID); r.Status != store.StatusFailed {
		t.Fatalf("r2: %+v", r)
	}
}

func TestRecoverKeepsStoppedServicesStopped(t *testing.T) {
	f := newFixture(t)
	r1, err := f.deploy(t, "v1", serverSpec())
	if err != nil {
		t.Fatal(err)
	}
	// Killed in `stop`: marked stopped, containers still running.
	f.st.UpdateService("shop", "server", func(v *store.Service) error { v.Stopped = true; return nil })
	if err := f.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.d.RunningWith("shop-server"); len(got) != 0 {
		t.Fatalf("running %v", got)
	}
	if _, ok := f.d.ByName[r1.Containers[0]]; !ok {
		t.Fatal("a stopped service lost its container")
	}

	// A container of the current release that disappeared is reported, not fatal.
	var events []string
	f.e.OnEvent = func(_ context.Context, _, _, kind string, failed bool, msg string) {
		events = append(events, kind+": "+msg)
	}
	f.st.UpdateService("shop", "server", func(v *store.Service) error { v.Stopped = false; return nil })
	f.d.ContainerRemove(context.Background(), r1.Containers[0])
	if err := f.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events %v", events)
	}
}
