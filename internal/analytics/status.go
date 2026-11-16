package analytics

import (
	"sort"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
)

// Status is GET /v1/projects/{project}/analytics/status.
type Status struct {
	Project    string                `json:"project"`
	Configured bool                  `json:"configured"`
	Config     *config.AnalyticsConf `json:"config,omitempty"`
	Secrets    []string              `json:"secrets,omitempty"` // names of the secrets set
	Services   []string              `json:"services,omitempty"`
	LastHour   int                   `json:"last_hour"` // records received
	Last24h    int                   `json:"last_24h"`
	LastRecord string                `json:"last_record,omitempty"`
	Window     Window                `json:"window"`
	DiskBytes  int64                 `json:"disk_bytes"`
	Rollups    int                   `json:"rollups"`
	Computed   string                `json:"rollups_computed,omitempty"`
	Archive    ArchiveStatus         `json:"archive"`
	Digest     string                `json:"digest_sent,omitempty"`
	Dropped    map[string]float64    `json:"dropped_24h,omitempty"` // by the server, analytics_dropped_total
	IndexDrops int64                 `json:"index_drops,omitempty"` // records the indexer could not take
}

// ArchiveStatus is the archive part of Status.
type ArchiveStatus struct {
	On          bool     `json:"on"`
	Problem     string   `json:"problem,omitempty"`
	LastDay     string   `json:"last_day,omitempty"`
	Missing     []string `json:"missing,omitempty"`
	LastOK      string   `json:"last_ok,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	LastErrorAt string   `json:"last_error_at,omitempty"`
	Snapshot    string   `json:"snapshot,omitempty"`
}

// Status describes the project's analytics.
func (p *Project) Status() Status {
	now := p.m.now()
	cfg := p.Config()
	s := Status{Project: p.name, Configured: cfg != nil, Config: cfg, Secrets: p.SecretsSet()}
	if p.m.Logs != nil {
		s.Services = p.m.Logs.Streams(p.name, logs.KindAnalytics)
		for _, svc := range s.Services {
			s.DiskBytes += p.m.Logs.DiskUsage(logs.Key{Project: p.name, Kind: logs.KindAnalytics, Name: svc})
		}
	}
	var last time.Time
	p.ReadRaw(now.Add(-24*time.Hour), time.Time{}, func(t time.Time, _ string) {
		s.Last24h++
		if now.Sub(t) <= time.Hour {
			s.LastHour++
		}
		if t.After(last) {
			last = t
		}
	})
	if !last.IsZero() {
		s.LastRecord = last.UTC().Format(time.RFC3339)
	}
	s.Window = Window{Since: p.WindowStart().Format(time.RFC3339), Until: now.Format(time.RFC3339)}
	s.Rollups = len(RollupDays(p.dir))
	p.state.GetMeta("rollups_computed", &s.Computed)
	p.state.GetMeta("digest_sent", &s.Digest)
	s.Dropped = p.Dropped(now.Add(-24*time.Hour), now)
	p.mu.Lock()
	s.IndexDrops = p.idx.dropped
	p.mu.Unlock()

	if cfg != nil && cfg.ArchiveOn() {
		s.Archive.On = true
		if _, _, err := p.S3(); err != nil {
			s.Archive.Problem = err.Error()
		}
		a := p.archiveState()
		s.Archive.LastOK, s.Archive.LastError, s.Archive.LastErrorAt = a.LastOK, a.LastError, a.LastErrorAt
		var done []string
		for d := range a.Raw {
			done = append(done, d)
		}
		sort.Strings(done)
		if len(done) > 0 {
			s.Archive.LastDay = done[len(done)-1]
		}
		ws := p.WindowStart().Truncate(24 * time.Hour)
		for d := ws; now.Sub(d) >= 48*time.Hour; d = d.AddDate(0, 0, 1) {
			if _, ok := a.Raw[d.Format(DayLayout)]; !ok {
				s.Archive.Missing = append(s.Archive.Missing, d.Format(DayLayout))
			}
		}
		var snaps []string
		for d := range a.State {
			snaps = append(snaps, d)
		}
		sort.Strings(snaps)
		if len(snaps) > 0 {
			s.Archive.Snapshot = snaps[len(snaps)-1]
		}
	}
	return s
}
