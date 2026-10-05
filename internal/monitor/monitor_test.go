package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

type fakeDocker struct {
	events  chan runtime.Event
	running map[string]bool
}

func (f *fakeDocker) Events(ctx context.Context, _ []string, _ ...string) (<-chan runtime.Event, <-chan error) {
	errc := make(chan error, 1)
	out := make(chan runtime.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			case ev := <-f.events:
				out <- ev
			}
		}
	}()
	return out, errc
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (runtime.ContainerInspect, error) {
	var ci runtime.ContainerInspect
	ci.State.Running = f.running[id]
	ci.NetworkSettings.Networks = map[string]struct {
		IPAddress string
		Aliases   []string
	}{"clowdr-shop": {IPAddress: id}}
	return ci, nil
}

type prober struct {
	mu   sync.Mutex
	down bool
}

func (p *prober) Probe(context.Context, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return errors.New("connection refused")
	}
	return nil
}

type fixture struct {
	m      *Monitor
	d      *fakeDocker
	p      *prober
	mu     sync.Mutex
	alerts []string
}

func (f *fixture) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.alerts...)
}

func newFixture(t *testing.T) *fixture {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ls := &logs.Store{BufDir: filepath.Join(dir, "buf"), DataDir: filepath.Join(dir, "logs"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ls.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	st.PutProject(store.Project{Name: "shop"})
	rel := &store.Release{Project: "shop", Service: "api", Status: store.StatusActive, Containers: []string{"shop-api-r1-1"}}
	st.CreateRelease(rel)
	st.UpdateService("shop", "api", func(v *store.Service) error {
		v.Current = rel.ID
		v.Spec = spec.Service{Name: "api", Port: 3000, Health: spec.Health{Path: "/health"}}
		return nil
	})
	f := &fixture{d: &fakeDocker{events: make(chan runtime.Event), running: map[string]bool{"shop-api-r1-1": true}}, p: &prober{}}
	plat, _ := config.ParsePlatform(nil)
	f.m = &Monitor{
		Docker: f.d, Store: st, Logs: ls, Prober: f.p, Platform: func() *config.Platform { return plat },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events: &Events{Logs: ls, Alert: func(_ context.Context, text string) {
			f.mu.Lock()
			f.alerts = append(f.alerts, text)
			f.mu.Unlock()
		}},
	}
	f.m.Init()
	return f
}

func ev(action, name string, rel uint64, attrs ...string) runtime.Event {
	var e runtime.Event
	e.Action = action
	e.Actor.Attributes = map[string]string{
		"name": name, runtime.LabelProject: "shop", runtime.LabelService: "api", runtime.LabelRole: "service",
		runtime.LabelRelease: fmt.Sprint(rel), runtime.LabelReplica: "1",
	}
	for i := 0; i+1 < len(attrs); i += 2 {
		e.Actor.Attributes[attrs[i]] = attrs[i+1]
	}
	return e
}

func TestCrashAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.m.Logs.Append(logs.Key{Project: "shop", Kind: logs.KindApp, Name: "api"}, logs.Line{T: time.Now().UnixNano(), R: 1, I: 1, E: true, L: "panic: nil map"})

	// A container of an old release dying is normal (drain).
	f.m.handle(ctx, ev("die", "shop-api-r0-1", 0, "exitCode", "143"))
	if len(f.sent()) != 0 {
		t.Fatal(f.sent())
	}
	f.m.handle(ctx, ev("die", "shop-api-r1-1", 1, "exitCode", "2"))
	got := f.sent()
	if len(got) != 1 || !strings.Contains(got[0], "exited with code 2") || !strings.Contains(got[0], "panic: nil map") {
		t.Fatalf("crash alert: %q", got)
	}
	// OOM.
	f.m.handle(ctx, ev("oom", "shop-api-r1-1", 1))
	f.m.handle(ctx, ev("die", "shop-api-r1-1", 1, "exitCode", "137"))
	if got := f.sent(); len(got) != 2 || !strings.Contains(got[1], "out of memory") {
		t.Fatalf("oom alert: %q", got)
	}
	// Restart loop: the 4th start within the window.
	for i := 0; i < 4; i++ {
		f.m.handle(ctx, ev("start", "shop-api-r1-1", 1))
	}
	if got := f.sent(); len(got) != 3 || !strings.Contains(got[2], "restarted 3 times") {
		t.Fatalf("restart loop: %q", got)
	}
	// Further crashes and restarts of the looping container are not reported again.
	for i := 0; i < 5; i++ {
		f.m.handle(ctx, ev("die", "shop-api-r1-1", 1, "exitCode", "2"))
		f.m.handle(ctx, ev("start", "shop-api-r1-1", 1))
	}
	if got := f.sent(); len(got) != 3 {
		t.Fatalf("loop noise: %q", got[3:])
	}
	// Recorded in the events journal.
	var kinds []string
	f.m.Events.Read("shop", logs.Query{}, func(e Event) { kinds = append(kinds, e.Kind) })
	if strings.Join(kinds, ",") != "crash,oom,restart-loop" {
		t.Fatal(kinds)
	}
}

func TestHealthAndRecovery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	svc, _ := f.m.Store.Service("shop", "api")
	f.p.down = true
	for i := 0; i < 5; i++ {
		f.m.probe(ctx, svc, "shop-api-r1-1")
	}
	if got := f.sent(); len(got) != 1 || !strings.Contains(got[0], "does not answer /health") {
		t.Fatalf("health: %q", got)
	}
	f.p.down = false
	f.m.probe(ctx, svc, "shop-api-r1-1")
	if got := f.sent(); len(got) != 2 || !strings.Contains(got[1], "answers health checks again") {
		t.Fatalf("recovered: %q", got)
	}
}

func TestRates(t *testing.T) {
	f := newFixture(t)
	acc := logs.Key{Project: "shop", Kind: logs.KindAccess, Name: "_"}
	for i := 0; i < 30; i++ {
		status := 200
		if i%5 == 0 {
			status = 502
		}
		f.m.OnLine(acc, logs.Line{L: fmt.Sprintf(`{"host":"api.example.com","status":%d}`, status)})
	}
	f.m.OnLine(acc, logs.Line{L: `{"host":"example.com","status":500}`}) // too few requests
	f.m.checkRates(context.Background())
	got := f.sent()
	if len(got) != 1 || !strings.Contains(got[0], "api.example.com: 6 of 30 requests failed") {
		t.Fatalf("5xx: %q", got)
	}
	f.m.checkRates(context.Background()) // counters reset
	if len(f.sent()) != 1 {
		t.Fatal("not reset")
	}
}

func TestDedup(t *testing.T) {
	var n int
	e := &Events{Alert: func(context.Context, string) { n++ }}
	for i := 0; i < 3; i++ {
		e.Emit(context.Background(), Event{Project: "p", Kind: "crash", Severity: Error, Message: "same"}, "")
	}
	e.Emit(context.Background(), Event{Project: "p", Kind: "deploy", Severity: Info, Message: "ok"}, "")
	e.Emit(context.Background(), Event{Project: "p", Kind: "health", Severity: Info, Message: "info only"}, "")
	if n != 2 {
		t.Fatal(n)
	}
}

func TestAccessFields(t *testing.T) {
	h, s := accessFields(`{"time":"x","host":"a.example.com","status":503,"bytes":1}`)
	if h != "a.example.com" || s != 503 {
		t.Fatal(h, s)
	}
}

// waitAlert waits until an alert containing s was sent.
func (f *fixture) waitAlert(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range f.sent() {
			if strings.Contains(a, s) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no alert with %q, got %q", s, f.sent())
}

func TestRunWatchers(t *testing.T) {
	f := newFixture(t)
	f.m.Every, f.m.HealthEvery = 20*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.m.Run(ctx); close(done) }()

	f.d.events <- ev("die", "shop-api-r1-1", 1, "exitCode", "1")
	f.waitAlert(t, "exited with code 1")

	f.p.mu.Lock()
	f.p.down = true
	f.p.mu.Unlock()
	f.waitAlert(t, "does not answer /health")

	for i := 0; i < 25; i++ {
		f.m.OnLine(logs.Key{Project: "shop", Kind: logs.KindAccess, Name: "_"}, logs.Line{L: `{"host":"api.example.com","status":503}`})
	}
	f.waitAlert(t, "25 of 25 requests failed")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestDiskAndMemory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const mb = 1 << 20
	f.m.Platform().Limits.DiskFreeAlert = 10

	// Old log segments are trimmed to get back above the threshold.
	k := logs.Key{Project: "shop", Kind: logs.KindApp, Name: "api"}
	dir := filepath.Join(f.m.Logs.DataDir, "shop", logs.KindApp, "api")
	os.MkdirAll(dir, 0o755)
	old := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Hour)
	for i := range 3 {
		os.WriteFile(filepath.Join(dir, old.Add(time.Duration(i)*time.Hour).Format("2006010215")+".zst"), make([]byte, 4096), 0o644)
	}
	before := f.m.Logs.DiskUsage(k)

	f.m.checkDisk(ctx, 50*mb, 1000*mb) // 5% free
	f.waitAlert(t, "only 5% disk free")
	if f.m.Logs.DiskUsage(k) >= before {
		t.Fatal("no old logs were removed on low disk")
	}
	n := len(f.sent())
	f.m.checkDisk(ctx, 500*mb, 1000*mb)
	f.m.checkDisk(ctx, 0, 0) // unknown
	f.m.checkMemory(ctx, 500*mb, 1000*mb)
	if len(f.sent()) != n {
		t.Fatalf("alerts without a reason: %q", f.sent()[n:])
	}
	f.m.checkMemory(ctx, 50*mb, 1000*mb)
	f.waitAlert(t, "only 50 MB of 1000 MB memory available")
}

func TestNginxDown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	down := 0
	f.m.NginxDown = func() { down++ }
	nginx := func(action string, attrs ...string) runtime.Event {
		var e runtime.Event
		e.Action = action
		e.Actor.Attributes = map[string]string{"name": "clowdr-nginx", runtime.LabelRole: "nginx"}
		for i := 0; i+1 < len(attrs); i += 2 {
			e.Actor.Attributes[attrs[i]] = attrs[i+1]
		}
		return e
	}
	f.m.handle(ctx, nginx("start"))
	if down != 0 || len(f.sent()) != 0 {
		t.Fatalf("start: %d %q", down, f.sent())
	}
	f.m.handle(ctx, nginx("die", "exitCode", "137"))
	f.m.handle(ctx, nginx("destroy"))
	got := f.sent()
	if down != 2 || len(got) != 2 || !strings.Contains(got[0], "code 137") || !strings.Contains(got[1], "removed") {
		t.Fatalf("down %d, alerts %q", down, got)
	}
	// A removed service container is not a crash.
	f.m.handle(ctx, ev("destroy", "shop-api-r1-1", 1))
	if len(f.sent()) != 2 {
		t.Fatal(f.sent())
	}
}
