package monitor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/runtime"
)

// Statser is the part of the docker client the container sampler needs.
type Statser interface {
	Stats(ctx context.Context, id string) (runtime.Stats, error)
}

// containerMetrics writes, once a minute, the CPU and memory of every running container of every
// service into that service's metric stream: the same points a service writes for itself
// (docs/metrics.md), so `clowctl metrics` shows them next to the application's own.
func (m *Monitor) containerMetrics(ctx context.Context) {
	sd, ok := m.Docker.(Statser)
	if !ok || m.Logs == nil {
		return
	}
	every := m.Every
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.sampleContainers(ctx, sd, time.Now())
	}
}

func (m *Monitor) sampleContainers(ctx context.Context, sd Statser, now time.Time) {
	svcs, err := m.Store.Services("")
	if err != nil {
		return
	}
	ts := now.UTC().Format(time.RFC3339)
	for _, v := range svcs {
		if v.Stopped {
			continue
		}
		rel, err := m.Store.Release(v.Project, v.Name, v.Current)
		if err != nil {
			continue
		}
		key := logs.Key{Project: v.Project, Kind: logs.KindMetric, Name: v.Name}
		for i, c := range rel.Containers {
			st, err := sd.Stats(ctx, c)
			if err != nil {
				continue
			}
			var lines []logs.Line
			for _, p := range []struct {
				name  string
				value float64
			}{
				{"container_cpu_percent", st.CPUPercent},
				{"container_memory_bytes", float64(st.MemoryUsage)},
				{"container_memory_limit_bytes", float64(st.MemoryLimit)},
			} {
				b, _ := json.Marshal(map[string]any{"$": "metric", "ts": ts, "name": p.name, "kind": "gauge", "value": p.value})
				lines = append(lines, logs.Line{T: now.UnixNano(), R: rel.ID, I: i + 1, L: string(b)})
			}
			m.Logs.Append(key, lines...)
		}
	}
}
