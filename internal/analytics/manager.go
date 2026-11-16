package analytics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/notify"
)

// Secret names, set with `clowctl analytics secret set`.
const (
	SecretS3Key         = "s3-key"
	SecretS3Secret      = "s3-secret"
	SecretS3Prefix      = "s3-prefix"
	SecretTelegramToken = "telegram-token"
	SecretTelegramChat  = "telegram-chat"
)

// SecretNames lists the secrets a project can set.
var SecretNames = []string{SecretS3Key, SecretS3Secret, SecretS3Prefix, SecretTelegramToken, SecretTelegramChat}

// Sender delivers a Telegram message (HTML) and reports failure.
type Sender interface {
	Deliver(ctx context.Context, text string) error
}

// Manager runs analytics for every project: indexing as lines arrive, rollups, the archive, the
// digest and install notifications on a schedule.
type Manager struct {
	// Dir holds one directory per project: state.db and days/.
	Dir string
	// Logs is where the raw records are (the analytics streams), with the access log and metrics.
	Logs *logs.Store
	// Retention is the hot window of raw records (platform.yml logs.analytics).
	Retention func() time.Duration
	// RollupCache is the byte budget of decoded rollups kept for queries (platform.yml
	// analytics.rollup_cache); nil or 0 is DefaultRollupCache.
	RollupCache func() int64
	// AllowPrivate lists archive endpoints (host:port) that may resolve to private, loopback
	// or link-local addresses (platform.yml analytics.archive.allow_private): an S3 on the same
	// host. Any other endpoint that does is refused when dialled.
	AllowPrivate func() []string
	// Exists reports whether a project still exists; the schedule skips deleted ones (their
	// directory stays until removed by hand). Nil: every project exists.
	Exists func(project string) bool
	Log    *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
	// HTTP is the client for S3 (tests point it at a fake).
	HTTP *http.Client
	// Telegram makes a sender for a bot token and chat (tests replace it).
	Telegram func(token, chat string) Sender

	mu       sync.Mutex
	guarded  *http.Client // the S3 client: public addresses only (netguard.go)
	projects map[string]*Project
	closed   bool
}

// Project is one project's analytics.
type Project struct {
	m     *Manager
	name  string
	dir   string
	state *State
	idx   *indexer

	mu         sync.Mutex
	lastQuick  time.Time            // rollups of today and yesterday
	lastFull   time.Time            // rollups of the last 8 days
	lastArch   time.Time            // last archive attempt
	eachHour   string               // the hour the per-install count below belongs to
	eachSent   int                  // per-install messages sent in eachHour
	eachMissed map[string]int       // hour → installs over the per-message limit
	noRollup   map[string]time.Time // day → when a query last found nothing to roll up
	restoreAt  time.Time            // next look into the archive for a lost index
	rollupMu   sync.Mutex
	indexMu    sync.Mutex
	archMu     sync.Mutex // raw days of the archive: the daily upload and an import
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) log() *slog.Logger {
	if m.Log == nil {
		return slog.Default()
	}
	return m.Log
}

func (m *Manager) retention() time.Duration {
	if m.Retention != nil {
		if r := m.Retention(); r > 0 {
			return r
		}
	}
	return 35 * 24 * time.Hour
}

func (m *Manager) httpClient() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guarded == nil {
		m.guarded = guardedClient(m.AllowPrivate, 5*time.Minute)
	}
	return m.guarded
}

func (m *Manager) sender(token, chat string) Sender {
	if m.Telegram != nil {
		return m.Telegram(token, chat)
	}
	return &notify.Telegram{Token: token, ChatID: chat, ParseMode: "HTML", Log: m.log(), Dedup: time.Second}
}

// Project opens (or returns) the analytics of a project.
func (m *Manager) Project(name string) (*Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("analytics: closed")
	}
	if m.projects == nil {
		m.projects = map[string]*Project{}
	}
	if p := m.projects[name]; p != nil {
		return p, nil
	}
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return nil, fmt.Errorf("analytics: bad project name %q", name)
	}
	dir := filepath.Join(m.Dir, name)
	st, err := OpenState(filepath.Join(dir, "state.db"))
	if err != nil {
		return nil, err
	}
	p := &Project{m: m, name: name, dir: dir, state: st, idx: &indexer{in: make(chan Record, 4096), sync: make(chan chan struct{}), done: make(chan struct{})}}
	go p.runIndexer()
	m.projects[name] = p
	return p, nil
}

// Close stops the indexers (after they flush) and closes every state.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	ps := m.projects
	m.projects = nil
	m.mu.Unlock()
	for _, p := range ps {
		p.close()
	}
}

func (p *Project) close() {
	close(p.idx.in)
	<-p.idx.done
	p.state.Close()
}

// OnLine takes a stored line from the collector; analytics records go to their project's indexer.
func (m *Manager) OnLine(k logs.Key, l logs.Line) {
	if k.Kind != logs.KindAnalytics || k.Project == logs.Platform {
		return
	}
	r, err := Parse(l.L)
	if err != nil {
		return
	}
	p, err := m.Project(k.Project)
	if err != nil {
		return
	}
	// Never block the collector: it serves every stream of the container, and docker's
	// non-blocking fluentd buffer drops the container's lines while it waits. The line is stored
	// already, so a full queue (a rebuild holds the index) only defers it: the indexer reads
	// the stream back from the first line it missed.
	select {
	case p.idx.in <- r:
	default:
		p.mu.Lock()
		p.idx.dropped++
		if t := l.Time(); p.idx.catchUp.IsZero() || t.Before(p.idx.catchUp) {
			p.idx.catchUp = t
		}
		p.mu.Unlock()
	}
}

// RenameProject moves a project's analytics to its new name (clowctl project rename).
func (m *Manager) RenameProject(old, name string) {
	m.mu.Lock()
	p := m.projects[old]
	delete(m.projects, old)
	m.mu.Unlock()
	if p != nil {
		p.close()
	}
	from, to := filepath.Join(m.Dir, old), filepath.Join(m.Dir, name)
	if _, err := os.Stat(from); err != nil {
		return
	}
	if _, err := os.Stat(to); err == nil {
		m.log().Error("analytics: rename: target exists, kept the old directory", "from", from, "to", to)
		return
	}
	if err := os.Rename(from, to); err != nil {
		m.log().Error("analytics: rename", "err", err)
	}
}

// ─── Config and secrets ──────────────────────────────────────────────────────

// Config returns the project's analytics config, or nil when it has none.
func (p *Project) Config() *config.AnalyticsConf {
	var c config.AnalyticsConf
	if !p.state.GetMeta("config", &c) {
		return nil
	}
	return &c
}

// SetConfig stores the config; it reports whether it changed.
func (p *Project) SetConfig(c config.AnalyticsConf) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	old := p.Config()
	if old != nil && sameJSON(old, &c) {
		return false, nil
	}
	return true, p.state.SetMeta("config", c)
}

// DeleteConfig removes the config: rollups, archive and digest stop; the stream stays.
func (p *Project) DeleteConfig() error {
	return p.state.Update(func(t *Tx) error { return t.tx.Bucket(bMeta).Delete([]byte("config")) })
}

func (p *Project) secrets() map[string]string {
	s := map[string]string{}
	p.state.GetMeta("secrets", &s)
	return s
}

// SecretsSet lists the names of the secrets that are set.
func (p *Project) SecretsSet() []string {
	var out []string
	for k, v := range p.secrets() {
		if v != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// SetSecret sets (or with "" removes) a secret.
func (p *Project) SetSecret(name, value string) error {
	known := false
	for _, n := range SecretNames {
		known = known || n == name
	}
	if !known {
		return fmt.Errorf("unknown secret %q (have: %s)", name, strings.Join(SecretNames, ", "))
	}
	value = strings.TrimSpace(value)
	if name == SecretS3Prefix {
		value = strings.Trim(value, "/")
	}
	return p.state.Update(func(t *Tx) error {
		s := map[string]string{}
		t.Meta("secrets", &s)
		if value == "" {
			delete(s, name)
		} else {
			s[name] = value
		}
		return t.PutMeta("secrets", s)
	})
}

// ─── Raw records ─────────────────────────────────────────────────────────────

// ReadRaw calls fn for every analytics record of the project received in [since, until)
// (zero until = open), stream by stream.
func (p *Project) ReadRaw(since, until time.Time, fn func(received time.Time, line string)) error {
	if p.m.Logs == nil {
		return nil
	}
	for _, svc := range p.m.Logs.Streams(p.name, logs.KindAnalytics) {
		k := logs.Key{Project: p.name, Kind: logs.KindAnalytics, Name: svc}
		err := p.m.Logs.Read(k, logs.Query{Since: since, Until: until}, func(l logs.Line) {
			if !until.IsZero() && !l.Time().Before(until) {
				return
			}
			fn(l.Time(), l.L)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// readRecords parses the raw records received in [since, until).
func (p *Project) readRecords(since, until time.Time, keep func(*Record) bool) ([]Record, error) {
	return p.readRecordsCtx(context.Background(), since, until, keep, 0)
}

// ErrTooManyRecords is a raw query that would hold more records than maxRaw in memory.
var ErrTooManyRecords = errors.New("the query matches too many raw records: narrow --since, name the events or filter by user")

// readRecordsCtx is readRecords that stops when ctx ends and, with limit > 0, refuses to keep
// more than limit records (the window of a busy project does not fit in memory).
func (p *Project) readRecordsCtx(ctx context.Context, since, until time.Time, keep func(*Record) bool, limit int) ([]Record, error) {
	var out []Record
	var stop error
	n := 0
	err := p.ReadRaw(since, until, func(_ time.Time, line string) {
		if stop != nil {
			return
		}
		if n++; n%4096 == 0 && ctx.Err() != nil {
			stop = ctx.Err()
			return
		}
		r, err := Parse(line)
		if err == nil && (keep == nil || keep(&r)) {
			if limit > 0 && len(out) >= limit {
				stop = ErrTooManyRecords
				return
			}
			out = append(out, r)
		}
	})
	if err == nil {
		err = stop
	}
	if err == nil {
		err = ctx.Err()
	}
	return out, err
}

// WindowStart is where the hot window of raw records begins.
func (p *Project) WindowStart() time.Time {
	start := p.m.now().Add(-p.m.retention())
	if p.m.Logs != nil {
		var oldest time.Time
		for _, svc := range p.m.Logs.Streams(p.name, logs.KindAnalytics) {
			o := p.m.Logs.Oldest(logs.Key{Project: p.name, Kind: logs.KindAnalytics, Name: svc})
			if !o.IsZero() && (oldest.IsZero() || o.Before(oldest)) {
				oldest = o
			}
		}
		if !oldest.IsZero() && oldest.After(start) {
			start = oldest
		}
	}
	return start
}

// streamFrom is where the raw records on disk begin: the window start, or now when the stream
// holds nothing yet (a new disk).
func (p *Project) streamFrom() time.Time {
	if p.m.Logs != nil {
		for _, svc := range p.m.Logs.Streams(p.name, logs.KindAnalytics) {
			if !p.m.Logs.Oldest(logs.Key{Project: p.name, Kind: logs.KindAnalytics, Name: svc}).IsZero() {
				return p.WindowStart()
			}
		}
	}
	return p.m.now()
}

// ─── Rollups ─────────────────────────────────────────────────────────────────

// lateDays is how far back an event may arrive and still count in its day's rollup: the
// client's offline queue keeps events for 7 days, plus one for clock shifts.
const lateDays = 8

// Recompute rebuilds the rollups of the given days (event days, UTC) from the raw records and
// returns the days whose rollup changed.
func (p *Project) Recompute(days []string) ([]string, error) {
	if len(days) == 0 {
		return nil, nil
	}
	p.rollupMu.Lock()
	defer p.rollupMu.Unlock()
	sort.Strings(days)
	for _, d := range days {
		if _, err := time.Parse(DayLayout, d); err != nil {
			return nil, err
		}
	}
	exclude := map[string]any{"test": true}
	if c := p.Config(); c != nil {
		exclude = c.ExcludeRule()
	}
	var changed []string
	now := p.m.now()
	windowStart := p.streamFrom()
	for _, d := range days {
		// One day at a time: memory holds one day's records, not the whole late window.
		recs, err := p.dayRecords(d)
		if err != nil {
			return changed, err
		}
		if len(recs) == 0 {
			if old, _ := LoadRollup(p.dir, d); old == nil {
				continue // nothing then, nothing now
			}
		}
		roll, err := ComputeDay(p.state, d, recs, exclude, now)
		if err != nil {
			return changed, err
		}
		// A day the stream does not hold from its start (a new disk, a restored index, a
		// window trimmed by age) is recomputed from part of its records: a rollup that counts
		// fewer events than the one on disk lost data, and the one on disk stays.
		if dayTime(d).Before(windowStart) {
			if old, _ := LoadRollup(p.dir, d); old != nil && old.eventCount() > roll.eventCount() {
				continue
			}
		}
		ok, err := SaveRollup(p.dir, roll)
		if err != nil {
			return changed, err
		}
		if ok {
			changed = append(changed, d)
		}
	}
	if len(changed) > 0 {
		var pending []string
		p.state.GetMeta("rollups_pending_upload", &pending)
		pending = mergeDays(pending, changed)
		p.state.SetMeta("rollups_pending_upload", pending)
	}
	p.state.SetMeta("rollups_computed", now.Format(time.RFC3339))
	return changed, nil
}

// dayRecords reads the records of one event day (UTC). An event is received after its day began
// (at most minutes before, by clock skew the server clamps), so the stream is read from an hour
// before the day. A line is parsed only when its ts names the day or a neighbour (an offset
// moves the UTC day by less than one), which keeps a pass over a busy week cheap.
func (p *Project) dayRecords(day string) ([]Record, error) {
	d := dayTime(day)
	marks := []string{
		`"ts":"` + d.AddDate(0, 0, -1).Format(DayLayout),
		`"ts":"` + day,
		`"ts":"` + d.AddDate(0, 0, 1).Format(DayLayout),
	}
	var out []Record
	err := p.ReadRaw(d.Add(-time.Hour), time.Time{}, func(_ time.Time, line string) {
		hit := !strings.Contains(line, `"ts":"`) // not compact JSON: parse it to know
		for _, m := range marks {
			hit = hit || strings.Contains(line, m)
		}
		if !hit {
			return
		}
		if r, err := Parse(line); err == nil && r.Day() == day {
			out = append(out, r)
		}
	})
	return out, err
}

func mergeDays(a, b []string) []string {
	m := map[string]bool{}
	for _, d := range append(a, b...) {
		m[d] = true
	}
	out := make([]string, 0, len(m))
	for d := range m {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// daysBack lists n days ending today, oldest first.
func daysBack(now time.Time, n int) []string {
	out := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, now.AddDate(0, 0, -i).Format(DayLayout))
	}
	return out
}

// ─── Schedule ────────────────────────────────────────────────────────────────

// Projects lists the projects with analytics: those with a directory or an analytics stream.
func (m *Manager) Projects() []string {
	seen := map[string]bool{}
	es, _ := os.ReadDir(m.Dir)
	for _, e := range es {
		if e.IsDir() {
			seen[e.Name()] = true
		}
	}
	if m.Logs != nil {
		ps, _ := os.ReadDir(m.Logs.DataDir)
		for _, e := range ps {
			if e.IsDir() && e.Name() != logs.Platform && len(m.Logs.Streams(e.Name(), logs.KindAnalytics)) > 0 {
				seen[e.Name()] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Run works the schedule until ctx ends: every minute it checks each project.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs whatever is due for every project.
func (m *Manager) Tick(ctx context.Context) {
	if m.RollupCache != nil {
		SetRollupCacheLimit(m.RollupCache())
	}
	for _, name := range m.Projects() {
		if m.Exists != nil && !m.Exists(name) {
			continue
		}
		p, err := m.Project(name)
		if err != nil {
			continue
		}
		p.tick(ctx)
	}
}

func (p *Project) tick(ctx context.Context) {
	cfg := p.Config()
	if cfg == nil {
		return
	}
	now := p.m.now()
	log := p.m.log().With("project", p.name)

	// A lost disk (or a new server): the index comes back from the archive before anything
	// is rolled up on top of an empty one.
	if cfg.ArchiveOn() {
		p.mu.Lock()
		due := !now.Before(p.restoreAt)
		if due {
			p.restoreAt = now.Add(time.Hour)
		}
		p.mu.Unlock()
		if due && p.state.Empty() {
			if _, _, err := p.S3(); err == nil {
				restored, err := p.Restore(ctx, func(msg string) { log.Info("analytics: restore", "step", msg) })
				if err != nil {
					log.Error("analytics: restore", "err", err)
				} else if restored {
					log.Info("analytics: index restored from the archive")
				}
			}
		}
	}

	p.mu.Lock()
	full := now.Sub(p.lastFull) >= time.Hour
	quick := now.Sub(p.lastQuick) >= 15*time.Minute
	p.mu.Unlock()
	switch {
	case full:
		if _, err := p.Recompute(daysBack(now, lateDays+1)); err != nil {
			log.Error("analytics: rollups", "err", err)
		}
		p.mu.Lock()
		p.lastFull, p.lastQuick = now, now
		p.mu.Unlock()
	case quick:
		if _, err := p.Recompute(daysBack(now, 2)); err != nil {
			log.Error("analytics: rollups", "err", err)
		}
		p.mu.Lock()
		p.lastQuick = now
		p.mu.Unlock()
	}

	if cfg.ArchiveOn() {
		p.mu.Lock()
		// Hourly (a failed upload retries), and right after 01:30 UTC, when yesterday is complete.
		at := now.Truncate(24 * time.Hour).Add(90 * time.Minute)
		due := now.Sub(p.lastArch) >= time.Hour || p.lastArch.Before(at) && !now.Before(at)
		if due {
			p.lastArch = now
		}
		p.mu.Unlock()
		if due {
			if err := p.ArchiveDue(ctx); err != nil {
				log.Warn("analytics: archive", "err", err)
			}
		}
	}

	if mode := cfg.InstallsMode(); mode == config.InstallsHourly || mode == config.InstallsEach {
		if err := p.hourlyInstalls(ctx, now); err != nil {
			log.Warn("analytics: hourly installs", "err", err)
		}
	}

	if cfg.Digest == nil || !cfg.Digest.Off {
		if now.Hour() >= cfg.DigestHour() {
			day := now.AddDate(0, 0, -1).Format(DayLayout)
			var last string
			p.state.GetMeta("digest_sent", &last)
			if last < day {
				if err := p.SendDigest(ctx, day); err != nil {
					log.Warn("analytics: digest", "err", err)
				} else {
					p.state.SetMeta("digest_sent", day)
				}
			}
		}
	}
}
