// Package metric reads the metric points services write to stdout (docs/metrics.md) and clowdrd
// writes for every container: one point per series per minute — a counter's increase, a gauge's
// value, a histogram's bucket counts. clowdrd stores them in a service's metric stream; this
// package lists and aggregates them over time and labels.
package metric

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	Counter   = "counter"
	Gauge     = "gauge"
	Histogram = "histogram"
)

// Point is one minute of one series.
type Point struct {
	TS     string            `json:"ts"`
	Name   string            `json:"name"`
	Kind   string            `json:"kind"`
	Labels map[string]string `json:"labels,omitempty"`
	// Value: the increase of a counter over the interval, the value of a gauge.
	Value float64 `json:"value,omitempty"`
	// Histogram: observations in the interval.
	Count  float64   `json:"count,omitempty"`
	Sum    float64   `json:"sum,omitempty"`
	Min    *float64  `json:"min,omitempty"`
	Max    *float64  `json:"max,omitempty"`
	Bounds []float64 `json:"bounds,omitempty"` // upper bounds; the last bucket is +Inf
	Counts []float64 `json:"counts,omitempty"` // len(Bounds)+1

	// Set by clowdrd from the stream.
	Service string `json:"service,omitempty"`
	Replica int    `json:"replica,omitempty"`
}

// Parse decodes one stored line; ok is false for anything that is not a metric point.
func Parse(line string) (Point, bool) {
	var p Point
	if json.Unmarshal([]byte(line), &p) != nil || p.Name == "" {
		return p, false
	}
	switch p.Kind {
	case Counter, Gauge, Histogram:
		return p, true
	}
	return p, false
}

func (p Point) Time() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, p.TS)
	return t
}

// label returns a label, with service and replica as pseudo-labels.
func (p Point) label(k string) string {
	switch k {
	case "service":
		return p.Service
	case "replica":
		return fmt.Sprint(p.Replica)
	}
	return p.Labels[k]
}

// seriesKey identifies one series of one writer (a replica writes its own points).
func (p Point) seriesKey() string {
	keys := make([]string, 0, len(p.Labels))
	for k := range p.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d", p.Service, p.Replica)
	for _, k := range keys {
		b.WriteString("|" + k + "=" + p.Labels[k])
	}
	return b.String()
}

// ─── Listing ──────────────────────────────────────────────────────────────────

// Info describes one metric name.
type Info struct {
	Name    string              `json:"name"`
	Kind    string              `json:"kind"`
	Service string              `json:"service,omitempty"`
	Series  int                 `json:"series"`
	Labels  map[string][]string `json:"labels,omitempty"` // label → values seen (at most 20)
	LastTS  string              `json:"lastTs"`
	Last    float64             `json:"last"` // the last value (gauge), points summed in the last minute (counter), count (histogram)
}

// Lister collects the metric names of a stream.
type Lister struct {
	infos  map[string]*Info
	series map[string]map[string]bool
}

func (l *Lister) Add(p Point) {
	if l.infos == nil {
		l.infos, l.series = map[string]*Info{}, map[string]map[string]bool{}
	}
	in := l.infos[p.Name]
	if in == nil {
		in = &Info{Name: p.Name, Kind: p.Kind, Service: p.Service, Labels: map[string][]string{}}
		l.infos[p.Name] = in
		l.series[p.Name] = map[string]bool{}
	}
	l.series[p.Name][p.seriesKey()] = true
	for k, v := range p.Labels {
		vs := in.Labels[k]
		if len(vs) < 20 && !contains(vs, v) {
			in.Labels[k] = append(vs, v)
		}
	}
	if p.TS >= in.LastTS {
		in.LastTS = p.TS
		switch p.Kind {
		case Histogram:
			in.Last = p.Count
		default:
			in.Last = p.Value
		}
	}
}

func (l *Lister) Result() []Info {
	out := make([]Info, 0, len(l.infos))
	for name, in := range l.infos {
		in.Series = len(l.series[name])
		for k := range in.Labels {
			sort.Strings(in.Labels[k])
		}
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// ─── Aggregation ──────────────────────────────────────────────────────────────

// Query aggregates the points of one metric into time buckets of Step, one series per distinct
// value of the By labels.
type Query struct {
	Name  string
	Step  time.Duration
	By    []string
	Where map[string]string // label filters
	// Agg: counter sum (default) | rate (per second); gauge max (default) | last | min | avg | sum;
	// histogram p95 (default) | p50 | p90 | p99 | avg | count | max | sum.
	Agg string
}

// Series is one aggregated line.
type Series struct {
	Labels map[string]string `json:"labels,omitempty"`
	Points []Sample          `json:"points"`
}

// Sample is one bucket: its start and value. Missing buckets are absent.
type Sample struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

type bucket struct {
	sum    float64
	byKey  map[string]Point // gauge: the last point of each series
	merged *Point           // histogram: merged buckets
}

// Aggregator folds points into a result.
type Aggregator struct {
	Q      Query
	kind   string
	groups map[string]map[int64]*bucket
	glabel map[string]map[string]string
}

func (a *Aggregator) Add(p Point) {
	if p.Name != a.Q.Name {
		return
	}
	for k, v := range a.Q.Where {
		if p.label(k) != v {
			return
		}
	}
	if a.groups == nil {
		a.groups, a.glabel = map[string]map[int64]*bucket{}, map[string]map[string]string{}
	}
	a.kind = p.Kind
	labels := map[string]string{}
	var gk strings.Builder
	for _, k := range a.Q.By {
		v := p.label(k)
		labels[k] = v
		gk.WriteString(k + "=" + v + "|")
	}
	g := a.groups[gk.String()]
	if g == nil {
		g = map[int64]*bucket{}
		a.groups[gk.String()] = g
		a.glabel[gk.String()] = labels
	}
	step := a.Q.Step
	if step <= 0 {
		step = time.Hour
	}
	t := p.Time().Truncate(step).Unix()
	b := g[t]
	if b == nil {
		b = &bucket{byKey: map[string]Point{}}
		g[t] = b
	}
	switch p.Kind {
	case Counter:
		b.sum += p.Value
	case Gauge:
		if prev, ok := b.byKey[p.seriesKey()]; !ok || p.TS >= prev.TS {
			b.byKey[p.seriesKey()] = p
		}
	case Histogram:
		if b.merged == nil {
			cp := p
			cp.Counts = append([]float64(nil), p.Counts...)
			b.merged = &cp
		} else {
			mergeHistogram(b.merged, p)
		}
	}
}

func mergeHistogram(into *Point, p Point) {
	into.Count += p.Count
	into.Sum += p.Sum
	if p.Min != nil && (into.Min == nil || *p.Min < *into.Min) {
		v := *p.Min
		into.Min = &v
	}
	if p.Max != nil && (into.Max == nil || *p.Max > *into.Max) {
		v := *p.Max
		into.Max = &v
	}
	if sameBounds(into.Bounds, p.Bounds) && len(into.Counts) == len(p.Counts) {
		for i := range p.Counts {
			into.Counts[i] += p.Counts[i]
		}
	} else {
		into.Bounds, into.Counts = nil, nil // incompatible buckets: quantiles are not computable
	}
}

func sameBounds(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Kind is the metric's kind, known after the first point.
func (a *Aggregator) Kind() string { return a.kind }

func (a *Aggregator) Result() []Series {
	step := a.Q.Step
	if step <= 0 {
		step = time.Hour
	}
	out := []Series{}
	for gk, g := range a.groups {
		ts := make([]int64, 0, len(g))
		for t := range g {
			ts = append(ts, t)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		s := Series{Labels: a.glabel[gk]}
		for _, t := range ts {
			v, ok := a.value(g[t], step)
			if !ok {
				continue
			}
			s.Points = append(s.Points, Sample{T: time.Unix(t, 0).UTC().Format(time.RFC3339), V: v})
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i].Labels) < fmt.Sprint(out[j].Labels) })
	return out
}

func (a *Aggregator) value(b *bucket, step time.Duration) (float64, bool) {
	switch a.kind {
	case Counter:
		if a.Q.Agg == "rate" {
			return b.sum / step.Seconds(), true
		}
		return b.sum, true
	case Gauge:
		if len(b.byKey) == 0 {
			return 0, false
		}
		vals := make([]float64, 0, len(b.byKey))
		var last Point
		for _, p := range b.byKey {
			vals = append(vals, p.Value)
			if p.TS >= last.TS {
				last = p
			}
		}
		switch a.Q.Agg {
		case "last":
			return last.Value, true
		case "min":
			return reduce(vals, math.Min), true
		case "sum":
			return reduce(vals, func(x, y float64) float64 { return x + y }), true
		case "avg":
			return reduce(vals, func(x, y float64) float64 { return x + y }) / float64(len(vals)), true
		default:
			return reduce(vals, math.Max), true
		}
	case Histogram:
		h := b.merged
		if h == nil || h.Count == 0 {
			return 0, false
		}
		switch a.Q.Agg {
		case "count":
			return h.Count, true
		case "sum":
			return h.Sum, true
		case "avg":
			return h.Sum / h.Count, true
		case "max":
			if h.Max != nil {
				return *h.Max, true
			}
			return quantile(h, 1)
		case "p50":
			return quantile(h, 0.5)
		case "p90":
			return quantile(h, 0.9)
		case "p99":
			return quantile(h, 0.99)
		default:
			return quantile(h, 0.95)
		}
	}
	return 0, false
}

func reduce(vs []float64, f func(a, b float64) float64) float64 {
	r := vs[0]
	for _, v := range vs[1:] {
		r = f(r, v)
	}
	return r
}

// quantile estimates a quantile from bucket counts by linear interpolation inside the bucket,
// bounded by the observed min and max.
func quantile(h *Point, q float64) (float64, bool) {
	if len(h.Counts) == 0 || len(h.Counts) != len(h.Bounds)+1 {
		return 0, false
	}
	rank := q * h.Count
	var seen float64
	for i, c := range h.Counts {
		if c == 0 {
			continue
		}
		if seen+c >= rank {
			lo := 0.0
			if i > 0 {
				lo = h.Bounds[i-1]
			} else if h.Min != nil {
				lo = *h.Min
			}
			var hi float64
			if i < len(h.Bounds) {
				hi = h.Bounds[i]
			} else if h.Max != nil {
				hi = *h.Max
			} else {
				hi = lo
			}
			if h.Max != nil && hi > *h.Max {
				hi = *h.Max
			}
			if h.Min != nil && lo < *h.Min {
				lo = *h.Min
			}
			return lo + (hi-lo)*((rank-seen)/c), true
		}
		seen += c
	}
	if h.Max != nil {
		return *h.Max, true
	}
	return 0, false
}
