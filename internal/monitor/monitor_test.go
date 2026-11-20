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

	mu        sync.Mutex
	oomKilled map[string]bool
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
	f.mu.Lock()
	ci.State.OOMKilled = f.oomKilled[id]
	f.mu.Unlock()
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
	kernel chan func(id string) // the found callback of OOMKills once Run starts it
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
	f := &fixture{d: &fakeDocker{events: make(chan runtime.Event), running: map[string]bool{"shop-api-r1-1": true}}, p: &prober{},
		kernel: make(chan func(string), 1)}
	plat, _ := config.ParsePlatform(nil)
	f.m = &Monitor{
		Docker: f.d, Store: st, Logs: ls, Prober: f.p, Platform: func() *config.Platform { return plat },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OOMKills: func(ctx context.Context, found func(string)) error {
			f.kernel <- found
			<-ctx.Done()
			return nil
		},
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
	e.Actor.ID = name // docker gives the container id; any stable key will do here
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

// The "oom" event of an exit can come before or after its "die" (containerd publishes the
// OOM kill from a cgroup watcher, apart from the exit), and inspect right after the exit can
// still show OOMKilled=false.
func TestOOMOrdering(t *testing.T) {
	ctx := context.Background()
	const name = "shop-api-r1-1"
	kinds := func(f *fixture) string {
		var out []string
		f.m.Events.Read("shop", logs.Query{}, func(e Event) { out = append(out, e.Kind) })
		return strings.Join(out, ",")
	}
	// settle waits past the grace so that a late alert would have been sent.
	settle := func(f *fixture) { time.Sleep(f.m.OOMGrace + 50*time.Millisecond) }

	t.Run("oom before die", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = 20 * time.Millisecond
		f.m.handle(ctx, ev("oom", name, 1))
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		if got := f.sent(); len(got) != 1 || !strings.Contains(got[0], "out of memory") {
			t.Fatalf("oom alert: %q", got)
		}
		settle(f)
		if got := kinds(f); got != "oom" {
			t.Fatal(got)
		}
	})

	t.Run("die before oom", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = time.Minute
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		if got := f.sent(); len(got) != 0 {
			t.Fatalf("reported before the oom event could come: %q", got)
		}
		f.m.handle(ctx, ev("oom", name, 1))
		if got := f.sent(); len(got) != 1 || !strings.Contains(got[0], "out of memory") {
			t.Fatalf("oom alert: %q", got)
		}
		// The event was used up: the next exit of the restarted container is a crash.
		f.m.handle(ctx, ev("start", name, 1))
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "1"))
		if got := kinds(f); got != "oom,crash" {
			t.Fatal(got)
		}
		f.m.mu.Lock()
		left := len(f.m.oom) + len(f.m.pending)
		f.m.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d entries left", left)
		}
	})

	t.Run("inspect already shows the oom kill", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = time.Minute
		f.d.oomKilled = map[string]bool{name: true}
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		if got := f.sent(); len(got) != 1 || !strings.Contains(got[0], "out of memory") {
			t.Fatalf("oom alert: %q", got)
		}
	})

	t.Run("inspect shows the oom kill by the end of the grace", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = 20 * time.Millisecond
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		f.d.mu.Lock()
		f.d.oomKilled = map[string]bool{name: true}
		f.d.mu.Unlock()
		f.waitAlert(t, "out of memory")
	})

	t.Run("a kill is a crash", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = 20 * time.Millisecond
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		f.waitAlert(t, "exited with code 137")
		settle(f)
		if got := kinds(f); got != "crash" {
			t.Fatal(got)
		}
	})

	t.Run("an oom later than the grace does not label the next exit", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = 20 * time.Millisecond
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		f.waitAlert(t, "exited with code 137")
		f.m.handle(ctx, ev("oom", name, 1))
		f.m.handle(ctx, ev("start", name, 1))
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "1"))
		settle(f)
		if got := kinds(f); got != "crash,crash" {
			t.Fatal(got)
		}
	})

	t.Run("an oom later than the grace is dropped with the container", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = 20 * time.Millisecond
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		f.waitAlert(t, "exited with code 137")
		f.m.handle(ctx, ev("oom", name, 1))
		f.m.handle(ctx, ev("destroy", name, 1))
		f.m.mu.Lock()
		left := len(f.m.oom)
		f.m.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d oom entries left", left)
		}
	})

	t.Run("only the kernel log reports the kill", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = time.Minute
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		f.m.sawOOM(name)
		if got := f.sent(); len(got) != 1 || !strings.Contains(got[0], "out of memory") {
			t.Fatalf("oom alert: %q", got)
		}
		// Docker's own report of the same kill, if it comes, does not label the next run.
		f.m.handle(ctx, ev("oom", name, 1))
		f.m.handle(ctx, ev("start", name, 1))
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "1"))
		if got := kinds(f); got != "oom,crash" {
			t.Fatal(got)
		}
	})

	t.Run("kills in other containers are forgotten in time", func(t *testing.T) {
		f := newFixture(t)
		f.m.mu.Lock()
		f.m.oom["other"] = time.Now().Add(-2 * time.Hour)
		f.m.mu.Unlock()
		f.m.sawOOM("another")
		f.m.mu.Lock()
		_, old := f.m.oom["other"]
		_, recent := f.m.oom["another"]
		f.m.mu.Unlock()
		if old || !recent {
			t.Fatalf("old kept %v, recent kept %v", old, recent)
		}
	})

	t.Run("dies again before the oom event", func(t *testing.T) {
		f := newFixture(t)
		f.m.OOMGrace = time.Minute
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "137"))
		f.m.handle(ctx, ev("start", name, 1))
		f.m.handle(ctx, ev("die", name, 1, "exitCode", "2"))
		if got := kinds(f); got != "crash,crash" {
			t.Fatal(got)
		}
	})
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

func TestErrorRate(t *testing.T) {
	f := newFixture(t)
	f.m.MaxErrorsPerMin = 3
	web, api := logs.Key{Project: "shop", Kind: logs.KindApp, Name: "web"}, logs.Key{Project: "shop", Kind: logs.KindApp, Name: "api"}
	for i := 0; i < 50; i++ { // an access log on stderr is not an error
		f.m.OnLine(web, logs.Line{E: true, L: "[::ffff:172.18.0.1]:36688: response:200"})
	}
	for _, l := range []string{"ERROR db: connection refused", `{"level":"error","msg":"boom"}`, "TypeError: x is undefined"} {
		f.m.OnLine(api, logs.Line{L: l}) // errors on stdout count
	}
	f.m.checkRates(context.Background())
	got := f.sent()
	if len(got) != 1 || !strings.Contains(got[0], "3 error lines") || !strings.Contains(got[0], "api") {
		t.Fatalf("alerts: %q", got)
	}
}

func TestErrorLine(t *testing.T) {
	for l, want := range map[string]bool{
		"[::ffff:172.18.0.1]:36688: response:200":                     false,
		"GET /health 200 1ms":                                         false,
		"level=info msg=\"retrying after error\"":                     false, // the level wins
		`{"level":"warn","msg":"slow query"}`:                         false,
		"processed 10 jobs, 0 errors":                                 false,
		"2026/10/07 15:35:46 [notice] 1#1: start worker processes":    false,
		"written to stderr by design":                                 false,
		"no terror here":                                              false,
		"level=error msg=\"db down\"":                                 true,
		`{"time":"x","level":"ERROR","msg":"db down"}`:                true,
		`{"severity":"CRITICAL","message":"disk"}`:                    true,
		"2026/10/07 15:35:46 [error] 7#7: *1 connect() failed":        true,
		"ERROR: relation \"users\" does not exist":                    true,
		"panic: runtime error: invalid memory address":                true,
		"Traceback (most recent call last):":                          true,
		"Exception in thread \"main\" java.lang.NullPointerException": true,
		"TypeError: Cannot read properties of undefined":              true,
		"fatal: unable to access repository":                          true,
	} {
		if got := errorLine(l); got != want {
			t.Errorf("errorLine(%q) = %v, want %v", l, got, want)
		}
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

	// An OOM kill that only the kernel log reports.
	f.m.mu.Lock()
	f.m.OOMGrace = time.Hour
	f.m.mu.Unlock()
	f.d.events <- ev("die", "shop-api-r1-1", 1, "exitCode", "137")
	(<-f.kernel)("shop-api-r1-1")
	f.waitAlert(t, "out of memory")

	f.p.mu.Lock()
	f.p.down = true
	f.p.mu.Unlock()
	f.waitAlert(t, "does not answer /health")

	// Still waiting for its OOM kill when the monitor stops: reported as a crash then.
	f.d.events <- ev("die", "shop-api-r1-1", 1, "exitCode", "137")

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
	f.waitAlert(t, "exited with code 137")
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

func TestOOMKilled(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	inner := strings.Repeat("cd34", 16)
	for _, c := range []struct{ record, want string }{
		{"3,812,4361683816,-;oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=docker-" + id + ".scope,mems_allowed=0,oom_memcg=/system.slice/docker-" + id + ".scope,task_memcg=/system.slice/docker-" + id + ".scope,task=tail,pid=78938,uid=0\n", id},
		{"3,9,1,-;oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=" + id + ",mems_allowed=0,oom_memcg=/docker/" + id + ",task_memcg=/docker/" + id + ",task=awk,pid=7,uid=0\n SUBSYSTEM=memory", id},
		// A global OOM has no oom_memcg; a nested cgroup gives every id on its path.
		{"3,10,2,-;oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/docker/" + id + "/system.slice/docker-" + inner + ".scope,task=java,pid=8,uid=0", id + "," + inner},
		{"3,11,3,-;Memory cgroup out of memory: Killed process 78938 (tail) total-vm:67000kB", ""},
		{"6,12,4,-;eth0: link up, task_memcg=/docker/" + id, ""},
		{"no record", ""},
	} {
		if got := strings.Join(oomKilled(c.record), ","); got != c.want {
			t.Errorf("%.40q: got %q, want %q", c.record, got, c.want)
		}
	}
}
