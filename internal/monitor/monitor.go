package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/store"
)

type Docker interface {
	Events(ctx context.Context, actions []string, labels ...string) (<-chan runtime.Event, <-chan error)
	ContainerInspect(ctx context.Context, id string) (runtime.ContainerInspect, error)
}

type Prober interface {
	Probe(ctx context.Context, url string) error
}

type Monitor struct {
	Docker   Docker
	Store    *store.Store
	Logs     *logs.Store
	Events   *Events
	Prober   Prober
	Platform func() *config.Platform
	StateDir string
	Log      *slog.Logger

	// Every is how often rates, disk and memory are checked (default a minute).
	Every time.Duration
	// Thresholds (defaults in Run).
	HealthEvery     time.Duration
	HealthFailures  int
	RestartLoop     int // starts within RestartWindow
	RestartWindow   time.Duration
	Min5xxRequests  int
	Max5xxRatio     float64
	MaxStderrPerMin int

	mu       sync.Mutex
	starts   map[string][]time.Time // container -> start times
	oom      map[string]bool
	looping  map[string]time.Time // container -> when its restart loop was reported
	failures map[string]int       // container -> consecutive failed probes
	rates    map[rateKey]*rate
	once     sync.Once
}

type rateKey struct{ project, service, host string }

type rate struct{ total, bad int }

// Init sets defaults; call it before OnLine can be called. Run calls it too.
func (m *Monitor) Init() { m.once.Do(m.defaults) }

func (m *Monitor) defaults() {
	if m.Every == 0 {
		m.Every = time.Minute
	}
	if m.HealthEvery == 0 {
		m.HealthEvery = 30 * time.Second
	}
	if m.HealthFailures == 0 {
		m.HealthFailures = 3
	}
	if m.RestartLoop == 0 {
		m.RestartLoop = 3
	}
	if m.RestartWindow == 0 {
		m.RestartWindow = 10 * time.Minute
	}
	if m.Min5xxRequests == 0 {
		m.Min5xxRequests = 20
	}
	if m.Max5xxRatio == 0 {
		m.Max5xxRatio = 0.1
	}
	if m.MaxStderrPerMin == 0 {
		m.MaxStderrPerMin = 100
	}
	m.starts = map[string][]time.Time{}
	m.oom = map[string]bool{}
	m.looping = map[string]time.Time{}
	m.failures = map[string]int{}
	m.rates = map[rateKey]*rate{}
}

// Run starts all watchers and blocks until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	m.Init()
	var wg sync.WaitGroup
	for _, f := range []func(context.Context){m.watchDocker, m.watchHealth, m.watchRates, m.watchResources} {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	wg.Wait()
}

// OnLine counts 5xx responses and stderr lines (hook for logs.Collector).
func (m *Monitor) OnLine(k logs.Key, l logs.Line) {
	var rk rateKey
	bad := false
	switch k.Kind {
	case logs.KindAccess:
		host, status := accessFields(l.L)
		rk = rateKey{project: k.Project, host: host}
		bad = status >= 500
	case logs.KindApp:
		if !l.E {
			return
		}
		rk = rateKey{project: k.Project, service: k.Name}
		bad = true
	default:
		return
	}
	m.mu.Lock()
	if m.rates != nil {
		r := m.rates[rk]
		if r == nil {
			r = &rate{}
			m.rates[rk] = r
		}
		r.total++
		if bad {
			r.bad++
		}
	}
	m.mu.Unlock()
}

// accessFields extracts host and status from a json_combined line without a full decode.
func accessFields(s string) (host string, status int) {
	if i := strings.Index(s, `"host":"`); i >= 0 {
		rest := s[i+8:]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			host = rest[:j]
		}
	}
	if i := strings.Index(s, `"status":`); i >= 0 {
		rest := s[i+9:]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		status, _ = strconv.Atoi(rest[:j])
	}
	return
}

func (m *Monitor) watchRates(ctx context.Context) {
	t := time.NewTicker(m.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.checkRates(ctx)
	}
}

// checkRates evaluates the last minute's counters and starts a new minute.
func (m *Monitor) checkRates(ctx context.Context) {
	m.mu.Lock()
	rates := m.rates
	m.rates = map[rateKey]*rate{}
	m.mu.Unlock()
	for k, r := range rates {
		switch {
		case k.host != "" && r.total >= m.Min5xxRequests && float64(r.bad)/float64(r.total) >= m.Max5xxRatio:
			m.Events.Emit(ctx, Event{Project: k.project, Kind: "5xx", Severity: Warn,
				Message: fmt.Sprintf("%s: %d of %d requests failed with 5xx in the last minute", k.host, r.bad, r.total)}, "")
		case k.service != "" && r.bad >= m.MaxStderrPerMin:
			m.Events.Emit(ctx, Event{Project: k.project, Service: k.service, Kind: "errors", Severity: Warn,
				Message: fmt.Sprintf("%d lines on stderr in the last minute", r.bad)}, "")
		}
	}
}

// watchDocker follows container events, reconnecting when docker restarts.
func (m *Monitor) watchDocker(ctx context.Context) {
	for ctx.Err() == nil {
		evs, errc := m.Docker.Events(ctx, []string{"die", "oom", "start"}, runtime.LabelManaged)
		for ev := range evs {
			m.handle(ctx, ev)
		}
		if err := <-errc; err != nil && ctx.Err() == nil {
			m.Log.Warn("docker events", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (m *Monitor) handle(ctx context.Context, ev runtime.Event) {
	a := ev.Actor.Attributes
	name := a["name"]
	project, service, role := a[runtime.LabelProject], a[runtime.LabelService], a[runtime.LabelRole]
	if role == "addon" {
		service = a[runtime.LabelAddon]
	}
	if role == "nginx" {
		if ev.Action == "die" {
			m.Events.Emit(ctx, Event{Kind: "nginx", Severity: Error, Message: "nginx exited with code " + a["exitCode"] + " (docker restarts it)"}, "")
		}
		return
	}
	if project == "" {
		return
	}
	switch ev.Action {
	case "oom":
		m.mu.Lock()
		m.oom[name] = true
		m.mu.Unlock()
	case "start":
		now := time.Now()
		m.mu.Lock()
		var recent []time.Time
		for _, t := range m.starts[name] {
			if now.Sub(t) < m.RestartWindow {
				recent = append(recent, t)
			}
		}
		recent = append(recent, now)
		m.starts[name] = recent
		n := len(recent)
		m.mu.Unlock()
		if n > m.RestartLoop && !m.inLoop(name) {
			m.mu.Lock()
			m.looping[name] = now
			m.mu.Unlock()
			m.Events.Emit(ctx, Event{Project: project, Service: service, Kind: "restart-loop", Severity: Error,
				Message: fmt.Sprintf("%s keeps crashing: restarted %d times in %s", name, n-1, m.RestartWindow)}, m.tail(project, service, a))
		}
	case "die":
		if role == "service" && !m.isCurrent(project, service, name) {
			return // drained, replaced, failed deploy or stopped on purpose
		}
		if m.inLoop(name) {
			// Already reported as a restart loop; keep the logs, skip the noise.
			if role == "service" && m.Logs != nil {
				m.Logs.Flush(logs.Key{Project: project, Kind: logs.KindApp, Name: service})
			}
			return
		}
		m.mu.Lock()
		oom := m.oom[name]
		delete(m.oom, name)
		m.mu.Unlock()
		msg := fmt.Sprintf("%s exited with code %s", name, a["exitCode"])
		kind := "crash"
		if oom || a["exitCode"] == "137" && m.oomKilled(ctx, name) {
			kind = "oom"
			msg = fmt.Sprintf("%s was killed: out of memory", name)
		}
		if role == "service" && m.Logs != nil {
			m.Logs.Flush(logs.Key{Project: project, Kind: logs.KindApp, Name: service})
		}
		m.Events.Emit(ctx, Event{Project: project, Service: service, Kind: kind, Severity: Error, Message: msg}, m.tail(project, service, a))
	}
}

func (m *Monitor) oomKilled(ctx context.Context, name string) bool {
	ci, err := m.Docker.ContainerInspect(ctx, name)
	return err == nil && ci.State.OOMKilled
}

func (m *Monitor) isCurrent(project, service, container string) bool {
	svc, err := m.Store.Service(project, service)
	if err != nil || svc.Stopped {
		return false
	}
	rel, err := m.Store.Release(project, service, svc.Current)
	if err != nil {
		return false
	}
	for _, c := range rel.Containers {
		if c == container {
			return true
		}
	}
	return false
}

func (m *Monitor) tail(project, service string, a map[string]string) string {
	if m.Logs == nil || a[runtime.LabelRole] != "service" {
		return ""
	}
	rel, _ := strconv.ParseUint(a[runtime.LabelRelease], 10, 64)
	rep, _ := strconv.Atoi(a[runtime.LabelReplica])
	return strings.Join(m.Logs.Tail(project, service, rel, rep, 15), "\n")
}

// watchHealth probes running services; alerts after HealthFailures failures in a row and
// when they recover.
func (m *Monitor) watchHealth(ctx context.Context) {
	t := time.NewTicker(m.HealthEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		svcs, err := m.Store.Services("")
		if err != nil {
			continue
		}
		for _, v := range svcs {
			if v.Stopped || v.Current == 0 || v.Spec.Health.Path == "" || v.Spec.Port == 0 {
				continue
			}
			rel, err := m.Store.Release(v.Project, v.Name, v.Current)
			if err != nil {
				continue
			}
			for _, c := range rel.Containers {
				m.probe(ctx, v, c)
			}
		}
	}
}

func (m *Monitor) probe(ctx context.Context, v store.Service, container string) {
	ci, err := m.Docker.ContainerInspect(ctx, container)
	if err != nil || !ci.State.Running {
		return // crashes are reported by the docker watcher
	}
	ip := ci.NetworkSettings.Networks["clowdr-"+v.Project].IPAddress
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = m.Prober.Probe(pctx, fmt.Sprintf("http://%s:%d%s", ip, v.Spec.Port, v.Spec.Health.Path))
	cancel()
	m.mu.Lock()
	before := m.failures[container]
	if err == nil {
		delete(m.failures, container)
	} else {
		m.failures[container] = before + 1
	}
	after := m.failures[container]
	m.mu.Unlock()
	switch {
	case err != nil && after == m.HealthFailures:
		m.Events.Emit(ctx, Event{Project: v.Project, Service: v.Name, Kind: "health", Severity: Error,
			Message: fmt.Sprintf("%s does not answer %s: %v", container, v.Spec.Health.Path, err)}, "")
	case err == nil && before >= m.HealthFailures:
		m.Events.Emit(ctx, Event{Project: v.Project, Service: v.Name, Kind: "recovered", Severity: Info,
			Message: container + " answers health checks again"}, "")
	}
}

// watchResources checks disk and memory every minute.
func (m *Monitor) watchResources(ctx context.Context) {
	t := time.NewTicker(m.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if free, total, ok := diskFree(m.StateDir); ok {
			m.checkDisk(ctx, free, total)
		}
		if avail, total, ok := memAvailable(); ok {
			m.checkMemory(ctx, avail, total)
		}
	}
}

// checkDisk alerts below the platform's free-disk threshold; the oldest log segments go first
// to get back above it.
func (m *Monitor) checkDisk(ctx context.Context, free, total int64) {
	threshold := m.Platform().Limits.DiskFreeAlert
	if total <= 0 {
		return
	}
	pct := int(free * 100 / total)
	if pct >= threshold {
		return
	}
	freed := int64(0)
	if m.Logs != nil {
		freed = m.Logs.TrimOldest(int64(threshold)*total/100 - free)
	}
	m.Events.Emit(ctx, Event{Kind: "disk", Severity: Warn,
		Message: fmt.Sprintf("only %d%% disk free (%s of %s); removed %s of old logs", pct, config.FormatBytes(free&^(1<<20-1)), config.FormatBytes(total&^(1<<20-1)), config.FormatBytes(freed&^(1<<10-1)))}, "")
}

// checkMemory alerts when less than 10% of the memory is available.
func (m *Monitor) checkMemory(ctx context.Context, avail, total int64) {
	if total > 0 && avail*10 < total {
		m.Events.Emit(ctx, Event{Kind: "memory", Severity: Warn,
			Message: fmt.Sprintf("only %d MB of %d MB memory available", avail>>20, total>>20)}, "")
	}
}

// inLoop reports whether a restart loop of the container was reported within the window.
func (m *Monitor) inLoop(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.looping[name]
	if ok && time.Since(t) > m.RestartWindow {
		delete(m.looping, name)
		return false
	}
	return ok
}
