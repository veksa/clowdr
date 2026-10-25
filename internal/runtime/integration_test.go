package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

const testImage = "busybox:1.37"

// realDocker returns a client for the local docker engine, or skips the test.
func realDocker(t *testing.T) *Docker {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (-short)")
	}
	d, err := New()
	if err != nil {
		t.Skip("no docker:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.Info(ctx); err != nil {
		t.Skip("docker is not reachable:", err)
	}
	return d
}

func randomName(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// TestDockerEngine drives the client against a real engine: everything clowdrd does with a
// service container, from pull to removal.
func TestDockerEngine(t *testing.T) {
	d := realDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := randomName("clowdr-test")
	labels := map[string]string{"clowdr.test": id}
	t.Cleanup(func() {
		ctx := context.Background()
		cs, _ := d.Containers(ctx, "clowdr.test="+id)
		for _, c := range cs {
			d.ContainerRemove(ctx, c.ID)
		}
		d.NetworkRemove(ctx, id)
		d.NetworkRemove(ctx, id+"-b")
		d.VolumeRemove(ctx, id)
	})

	info, err := d.Info(ctx)
	if err != nil || info.ServerVersion == "" || info.MemTotal <= 0 {
		t.Fatalf("info: %+v %v", info, err)
	}

	if err := d.ImagePull(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	if err := d.ImagePull(ctx, "busybox:no-such-tag-clowdr"); err == nil {
		t.Fatal("pulling a missing tag must fail")
	}
	img, err := d.ImageInspect(ctx, testImage)
	if err != nil || img.ID == "" {
		t.Fatalf("image inspect: %+v %v", img, err)
	}
	if _, err := d.ImageInspect(ctx, "clowdr-test/missing:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
	imgs, err := d.Images(ctx, "busybox")
	if err != nil || len(imgs) == 0 {
		t.Fatalf("images: %v %v", imgs, err)
	}

	for range 2 { // idempotent
		if err := d.NetworkEnsure(ctx, id, labels); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.NetworkEnsure(ctx, id+"-b", labels); err != nil {
		t.Fatal(err)
	}
	if err := d.VolumeEnsure(ctx, id, labels); err != nil {
		t.Fatal(err)
	}
	if err := d.VolumeEnsure(ctx, id, labels); err != nil {
		t.Fatalf("ensure an existing volume again: %v", err)
	}
	if err := d.VolumeEnsure(ctx, id, map[string]string{LabelProject: "other"}); err == nil {
		t.Fatal("reused a volume that belongs to someone else")
	}
	vols, err := d.Volumes(ctx, "clowdr.test="+id)
	if err != nil || len(vols) != 1 || vols[0].Name != id {
		t.Fatalf("volumes: %+v %v", vols, err)
	}

	events, _ := d.Events(ctx, []string{"die"}, "clowdr.test="+id)

	cid, err := d.ContainerCreate(ctx, ContainerConfig{
		Name:     id,
		Image:    testImage,
		Cmd:      []string{"sh", "-c", `echo started; echo oops >&2; echo data > /data/f; exec httpd -f -p 8080`},
		Env:      []string{"MODE=test"},
		Labels:   labels,
		Memory:   32 << 20,
		Restart:  "no",
		Network:  id,
		Aliases:  []string{"web"},
		Mounts:   []Mount{{Type: "volume", Source: id, Target: "/data"}},
		Tmpfs:    map[string]string{"/tmp": "size=1m"},
		Pids:     64,
		NanoCPUs: 5e8,
		Hardened: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ContainerStart(ctx, cid); err != nil {
		t.Fatal(err)
	}

	ci, err := d.ContainerInspect(ctx, cid)
	if err != nil || !ci.State.Running || ci.Config.Labels["clowdr.test"] != id || ci.Name != "/"+id {
		t.Fatalf("inspect: %+v %v", ci, err)
	}
	if n, ok := ci.NetworkSettings.Networks[id]; !ok || n.IPAddress == "" {
		t.Fatalf("not on network %s: %+v", id, ci.NetworkSettings.Networks)
	}
	cs, err := d.Containers(ctx, "clowdr.test="+id)
	if err != nil || len(cs) != 1 || cs[0].Name() != id || cs[0].State != "running" {
		t.Fatalf("containers: %+v %v", cs, err)
	}

	out, code, err := d.Exec(ctx, cid, []string{"sh", "-c", "echo $MODE; cat /data/f"})
	if err != nil || code != 0 || out != "test\ndata\n" {
		t.Fatalf("exec: %q %d %v", out, code, err)
	}
	if out, _, err := d.Exec(ctx, cid, []string{"cat", "/sys/fs/cgroup/pids.max"}); err != nil || strings.TrimSpace(out) != "64" {
		t.Fatalf("pids.max: %q %v", out, err)
	}
	if _, code, err := d.Exec(ctx, cid, []string{"sh", "-c", "exit 3"}); err != nil || code != 3 {
		t.Fatalf("exec exit code: %d %v", code, err)
	}
	if out, _, _ := d.Exec(ctx, cid, []string{"sh", "-c", "wget -qO- http://web:8080/ >/dev/null 2>&1; echo $?"}); strings.TrimSpace(out) == "" {
		t.Fatal("exec: no output from wget")
	}

	// Logs: stdout and stderr are told apart.
	var stdout, stderr []string
	deadline := time.Now().Add(10 * time.Second)
	for len(stderr) == 0 && time.Now().Before(deadline) {
		stdout, stderr = nil, nil
		rc, err := d.ContainerLogs(ctx, cid, false, "all", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		Demux(rc, func(isErr bool, line string) {
			if isErr {
				stderr = append(stderr, line)
			} else {
				stdout = append(stdout, line)
			}
		})
		rc.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if len(stdout) == 0 || !strings.HasSuffix(stdout[0], "started") || len(stderr) == 0 || !strings.HasSuffix(stderr[0], "oops") {
		t.Fatalf("logs: stdout %q, stderr %q", stdout, stderr)
	}

	st, err := d.Stats(ctx, cid)
	if err != nil || st.MemoryUsage <= 0 || st.MemoryLimit != 32<<20 {
		t.Fatalf("stats: %+v %v", st, err)
	}

	if err := d.NetworkConnect(ctx, id+"-b", cid, []string{"web-b"}); err != nil {
		t.Fatal(err)
	}
	if err := d.NetworkConnect(ctx, id+"-b", cid, []string{"web-b"}); err != nil {
		t.Fatal("connecting twice must be a no-op:", err)
	}
	if err := d.NetworkDisconnect(ctx, id+"-b", cid); err != nil {
		t.Fatal(err)
	}

	if err := d.ContainerStop(ctx, cid, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.ContainerStop(ctx, cid, time.Second); err != nil {
		t.Fatal("stopping a stopped container must be a no-op:", err)
	}
	select {
	case ev := <-events:
		if ev.Action != "die" || ev.Actor.ID != cid || ev.Actor.Attributes["clowdr.test"] != id {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no die event")
	}

	if err := d.ContainerRemove(ctx, cid); err != nil {
		t.Fatal(err)
	}
	if err := d.ContainerRemove(ctx, cid); err != nil {
		t.Fatal("removing twice must be a no-op:", err)
	}
	if _, err := d.ContainerInspect(ctx, cid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed container: %v", err)
	}
	if err := d.NetworkRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.VolumeRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// A container over its memory limit is OOM-killed and inspect says so; clowdrd alerts on it.
func TestDockerOOM(t *testing.T) {
	d := realDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := d.ImagePull(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	id := randomName("clowdr-oom")
	cid, err := d.ContainerCreate(ctx, ContainerConfig{
		Name: id, Image: testImage, Memory: 8 << 20, Labels: map[string]string{"clowdr.test": id},
		// Holds ~64 MB in memory: far over the 8 MB limit.
		Cmd: []string{"sh", "-c", "head -c 67108864 /dev/zero | tail > /dev/null"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.ContainerRemove(context.Background(), cid) })
	if err := d.ContainerStart(ctx, cid); err != nil {
		t.Fatal(err)
	}
	for {
		ci, err := d.ContainerInspect(ctx, cid)
		if err != nil {
			t.Fatal(err)
		}
		if !ci.State.Running {
			if !ci.State.OOMKilled {
				t.Fatalf("exited %d without OOM", ci.State.ExitCode)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("still running")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
