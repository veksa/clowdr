package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/metric"
)

// MetricResult is one metric aggregated.
type MetricResult struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Step   string          `json:"step"`
	Agg    string          `json:"agg,omitempty"`
	Series []metric.Series `json:"series"`
}

func toPoint(k logs.Key, l logs.Line) (metric.Point, bool) {
	p, ok := metric.Parse(l.L)
	p.Service, p.Replica = k.Name, l.I
	return p, ok
}

// metricsList lists the metrics of a project: GET /v1/projects/{project}/metrics.
func (s *Server) metricsList(w http.ResponseWriter, r *http.Request) {
	keys, ok := s.serviceStreams(w, r, logs.KindMetric)
	if !ok {
		return
	}
	since, until, ok := window(w, r, 24*time.Hour)
	if !ok {
		return
	}
	var l metric.Lister
	for _, k := range keys {
		s.Logs.Read(k, logs.Query{Since: since, Until: until}, func(line logs.Line) {
			if p, ok := toPoint(k, line); ok {
				l.Add(p)
			}
		})
	}
	writeJSON(w, 200, l.Result())
}

// metricQuery aggregates one metric: GET /v1/projects/{project}/metrics/{name}?step=&by=&agg=&where=.
func (s *Server) metricQuery(w http.ResponseWriter, r *http.Request) {
	keys, ok := s.serviceStreams(w, r, logs.KindMetric)
	if !ok {
		return
	}
	since, until, ok := window(w, r, 24*time.Hour)
	if !ok {
		return
	}
	q := r.URL.Query()
	step := time.Hour
	if v := q.Get("step"); v != "" {
		d, err := parseAgo(v)
		if err != nil || d < time.Minute {
			writeErr(w, 400, "step: a duration of at least 1m, e.g. 5m, 1h, 1d")
			return
		}
		step = d
	}
	query := metric.Query{Name: r.PathValue("name"), Step: step, Agg: q.Get("agg"), Where: map[string]string{}}
	if v := q.Get("by"); v != "" {
		query.By = strings.Split(v, ",")
	}
	for _, kv := range strings.Split(q.Get("where"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			query.Where[k] = v
		}
	}
	a := &metric.Aggregator{Q: query}
	marker := `"name":"` + query.Name + `"`
	for _, k := range keys {
		s.Logs.Read(k, logs.Query{Since: since, Until: until, Match: func(l logs.Line) bool { return strings.Contains(l.L, marker) }}, func(line logs.Line) {
			if p, ok := toPoint(k, line); ok {
				a.Add(p)
			}
		})
	}
	if a.Kind() == "" {
		writeErr(w, 404, "no points of "+query.Name+" in this window (clowctl metrics ls)")
		return
	}
	writeJSON(w, 200, MetricResult{Name: query.Name, Kind: a.Kind(), Step: step.String(), Agg: query.Agg, Series: a.Result()})
}
