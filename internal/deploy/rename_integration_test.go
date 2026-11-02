package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy/fake"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

func dockerCmd(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// A rename against a real docker engine: what the fake cannot prove is that an image takes a second
// name and survives losing the first, and that a running container changes networks.
func TestRenameProjectRealDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (-short)")
	}
	d, err := runtime.New()
	if err != nil {
		t.Skip("no docker:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := d.Info(ctx); err != nil {
		t.Skip("docker is not reachable:", err)
	}
	if _, err := d.ContainerInspect(ctx, NginxContainer); err == nil {
		t.Skipf("a real %s runs on this host", NginxContainer)
	}

	var b [4]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	old, name, shared := "ren"+id, "new"+id, "exp"+id
	oldImage := "clowdr.local/" + old + "/web:v1"
	newImage := "clowdr.local/" + name + "/web:v1"
	sharedImage := "clowdr.local/" + shared + "/redis:v1"
	t.Cleanup(func() {
		ctx := context.Background()
		for _, p := range []string{old, name, shared} {
			cs, _ := d.Containers(ctx, runtime.LabelProject+"="+p)
			for _, c := range cs {
				d.ContainerRemove(ctx, c.ID)
			}
		}
		d.ContainerRemove(ctx, NginxContainer)
		for _, p := range []string{old, name, shared} {
			d.NetworkRemove(ctx, NetworkName(p))
		}
		for _, img := range []string{oldImage, newImage, sharedImage} {
			d.ImageRemove(ctx, img)
		}
	})

	dockerCmd(t, "FROM busybox:1.37\nCMD [\"sleep\",\"3600\"]\n", "build", "-q", "-t", oldImage, "-t", sharedImage, "-")
	dockerCmd(t, "", "run", "-d", "--name", NginxContainer, sharedImage)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plat, _ := config.ParsePlatform([]byte(config.DefaultPlatform))
	e := New(d, st, func() *config.Platform { return plat }, &ingress.Controller{Dir: dir, Nginx: &fake.Nginx{}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.CertExists = func(string) bool { return false }
	for _, p := range []string{old, shared} {
		if err := st.PutProject(store.Project{Name: p}); err != nil {
			t.Fatal(err)
		}
	}

	health := spec.Health{Interval: 100 * time.Millisecond, Timeout: 30 * time.Second, Successes: 2}
	web := spec.Service{Name: "web", Health: health, Drain: time.Millisecond, StopGrace: time.Second, AcceptFrom: []string{shared}}
	web.Defaults()
	rel, err := e.Deploy(ctx, Request{Project: old, Spec: web, Image: oldImage, Author: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	redis := spec.Service{Name: "redis", Health: health, StopGrace: time.Second, ExposeTo: []string{old}}
	redis.Defaults()
	rr, err := e.Deploy(ctx, Request{Project: shared, Spec: redis, Image: sharedImage, Author: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := d.ContainerInspect(ctx, rr.Containers[0])
	if err != nil {
		t.Fatal(err)
	}

	if err := e.RenameProject(ctx, old, name, "test", nil); err != nil {
		t.Fatal(err)
	}
	e.WaitDrains()

	if _, err := d.ContainerInspect(ctx, rel.Containers[0]); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("old container: %v", err)
	}
	ci, err := d.ContainerInspect(ctx, containerName(name, "web", rel.ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !ci.State.Running {
		t.Fatalf("new container is not running: %+v", ci.State)
	}
	if _, ok := ci.NetworkSettings.Networks[NetworkName(name)]; !ok {
		t.Fatalf("new container networks: %v", ci.NetworkSettings.Networks)
	}
	if _, err := d.ImageInspect(ctx, newImage); err != nil {
		t.Fatalf("new image name: %v", err)
	}
	if _, err := d.ImageInspect(ctx, oldImage); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("old image name: %v", err)
	}

	after, err := d.ContainerInspect(ctx, rr.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	if !after.State.Running || after.State.StartedAt != before.State.StartedAt {
		t.Fatalf("the exposed container was restarted: %+v -> %+v", before.State, after.State)
	}
	if _, ok := after.NetworkSettings.Networks[NetworkName(name)]; !ok {
		t.Errorf("the exposed container did not join the new network: %v", after.NetworkSettings.Networks)
	}
	if _, ok := after.NetworkSettings.Networks[NetworkName(old)]; ok {
		t.Errorf("the exposed container is still in the old network: %v", after.NetworkSettings.Networks)
	}
	if out, err := exec.Command("docker", "network", "inspect", NetworkName(old)).CombinedOutput(); err == nil {
		t.Errorf("old network is still there: %s", out)
	}

	// Name resolution between the two, the point of joining the network.
	if out := dockerCmd(t, "", "exec", ci.Name[1:], "nslookup", shared+"-redis", "127.0.0.11"); !strings.Contains(out, "Address") {
		t.Errorf("the service cannot resolve the exposed container:\n%s", out)
	}
}
