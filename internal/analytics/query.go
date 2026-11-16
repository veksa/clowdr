package analytics

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spec is a query: POST /v1/projects/{project}/analytics/query.
type Spec struct {
	Kind string `json:"kind"` // trends|funnel|retention|stickiness|lifecycle|paths|person|events|catalog|installs

	Events   []string `json:"events,omitempty"`   // trends: events; funnel: steps ("name" or "name:k=v,k=v")
	Event    string   `json:"event,omitempty"`    // stickiness, lifecycle, events: one event ("any" = any activity)
	Math     string   `json:"math,omitempty"`     // total|uniques|dau|wau|mau|per_user_avg|sum|avg|p50|p75|p90|p95|p99
	Prop     string   `json:"prop,omitempty"`     // the numeric property of sum/avg/p50/p90
	Interval string   `json:"interval,omitempty"` // hour|day|week|month
	Since    string   `json:"since,omitempty"`    // 30d, 2026-01-01 or RFC 3339
	Until    string   `json:"until,omitempty"`

	Breakdown string            `json:"breakdown,omitempty"` // a dimension, p.<prop>, person.<prop> or first_*; two keys as "a,b"
	Where     map[string]string `json:"where,omitempty"`     // the same keys; a key ending in "!" means "not equal"
	// Filters are filter expressions, all of which must hold: k=v, k!=v, k>v, k>=v, k<v, k<=v;
	// k=a|b (or k=a,b) is "one of", k!=a|b "none of", and the same key with = twice is "one of"
	// both values. They add to Where.
	Filters []string `json:"filters,omitempty"`
	Compare bool     `json:"compare,omitempty"` // trends: the previous period too

	IncludeTest bool `json:"include_test,omitempty"`

	Window string `json:"window,omitempty"` // funnel: conversion window, e.g. 7d
	Order  string `json:"order,omitempty"`  // funnel: ordered (default) | strict | any

	Start  string `json:"start,omitempty"`  // retention: "first" (first seen, default) or an event
	Return string `json:"return,omitempty"` // retention: "any" (default) or an event
	Period string `json:"period,omitempty"` // retention: day|week|month

	From  string `json:"from,omitempty"` // paths: from this screen
	To    string `json:"to,omitempty"`   // paths: to this screen
	Depth int    `json:"depth,omitempty"`

	User   string `json:"user,omitempty"`   // person, events
	Device string `json:"device,omitempty"` // events
	Limit  int    `json:"limit,omitempty"`
}

// Window is the time range a query covered.
type Window struct {
	Since string `json:"since"`
	Until string `json:"until"`
}

// Point is one bucket of a series.
type Point struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

// Series is one line of a trend.
type Series struct {
	Event     string  `json:"event"`
	Breakdown string  `json:"breakdown,omitempty"`
	Points    []Point `json:"points"`
	Previous  []Point `json:"previous,omitempty"`
	// Total is the whole range: the sum of the buckets for total and sum, the distinct actors
	// of the range for uniques, the events per one of them for per_user_avg, the value over
	// every number of the range for avg and the percentiles, and the average daily value for
	// dau, wau and mau, which TotalKind then says.
	Total     float64 `json:"total"`
	TotalKind string  `json:"total_kind,omitempty"` // "average" when Total is a daily average
}

// FunnelStep is one step of a funnel.
type FunnelStep struct {
	Step          string   `json:"step"`
	Count         int      `json:"count"`
	FromPrev      *float64 `json:"from_prev,omitempty"`  // share of the previous step, 0..1
	FromStart     *float64 `json:"from_start,omitempty"` // share of the first step, 0..1
	MedianSeconds *float64 `json:"median_seconds,omitempty"`
}

// Funnel is one funnel (one per breakdown value).
type Funnel struct {
	Breakdown string       `json:"breakdown,omitempty"`
	Steps     []FunnelStep `json:"steps"`
}

// Cohort is one retention cohort.
type Cohort struct {
	Start     string `json:"start"`
	Breakdown string `json:"breakdown,omitempty"`
	Size      int    `json:"size"`
	Values    []int  `json:"values"` // returned in period 0, 1, 2, ...
}

// Row is a generic table row.
type Row map[string]any

// PersonResult is one person.
type PersonResult struct {
	User    string            `json:"user"`
	Props   map[string]any    `json:"props,omitempty"`
	FirstTS string            `json:"first_ts,omitempty"`
	LastTS  string            `json:"last_ts,omitempty"`
	Devices []string          `json:"devices,omitempty"`
	Actor   *Actor            `json:"actor,omitempty"`
	Events  []json.RawMessage `json:"events,omitempty"`
}

// Result is the answer to a query. Source says what it was computed from: rollup (daily
// rollups), raw (the hot window of raw records) or mixed.
type Result struct {
	Kind         string   `json:"kind"`
	Source       string   `json:"source"`
	Window       Window   `json:"window"`
	ExcludedTest int      `json:"excluded_test"`
	Partial      bool     `json:"partial,omitempty"`
	NeedsArchive bool     `json:"needs_archive,omitempty"`
	Archive      []string `json:"archive,omitempty"`
	Note         string   `json:"note,omitempty"`
	Series       []Series `json:"series,omitempty"`
	Funnels      []Funnel `json:"funnels,omitempty"`
	Cohorts      []Cohort `json:"cohorts,omitempty"`
	Columns      []string `json:"columns,omitempty"`
	Rows         []Row    `json:"rows,omitempty"`
	// Screens are the top screens of a paths query (rows of screen, views, actors).
	ScreenColumns []string          `json:"screen_columns,omitempty"`
	Screens       []Row             `json:"screens,omitempty"`
	Person        *PersonResult     `json:"person,omitempty"`
	Events        []json.RawMessage `json:"events,omitempty"`
}

// ParseTime reads 30d, 12h, 2026-01-01 or an RFC 3339 time, relative to now.
func ParseTime(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(DayLayout, v); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil {
			return now.Add(-time.Duration(d) * 24 * time.Hour), nil
		}
	}
	if n, ok := strings.CutSuffix(v, "w"); ok {
		if d, err := strconv.Atoi(n); err == nil {
			return now.Add(-time.Duration(d) * 7 * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("bad time %q: use 30d, 12h, 2026-01-01 or RFC 3339", v)
}

// parseDur reads 7d, 2w or a Go duration.
func parseDur(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		d, err := strconv.Atoi(n)
		return time.Duration(d) * 24 * time.Hour, err
	}
	if n, ok := strings.CutSuffix(v, "w"); ok {
		d, err := strconv.Atoi(n)
		return time.Duration(d) * 7 * 24 * time.Hour, err
	}
	return time.ParseDuration(v)
}

type qctx struct {
	p            *Project
	spec         Spec
	now          time.Time
	since, until time.Time
	exclude      map[string]bool // excluded user ids
	excludedSeen map[string]bool
}

// Query answers a spec.
func (p *Project) Query(ctx context.Context, s Spec) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	q := &qctx{p: p, spec: s, now: p.m.now(), excludedSeen: map[string]bool{}}
	defSince := map[string]string{"events": "7d", "person": "35d", "catalog": "30d", "paths": "7d"}[s.Kind]
	if defSince == "" {
		defSince = "30d"
	}
	if s.Since == "" {
		s.Since = defSince
	}
	var err error
	if q.since, err = ParseTime(s.Since, q.now); err != nil {
		return nil, err
	}
	if q.until, err = ParseTime(s.Until, q.now); err != nil {
		return nil, err
	}
	if q.until.IsZero() || q.until.After(q.now) {
		q.until = q.now
	}
	if !q.since.Before(q.until) {
		return nil, errors.New("since must be before until")
	}
	q.spec = s
	switch s.Kind {
	case "retention", "stickiness", "lifecycle", "installs", "catalog", "person":
		if len(s.Where) > 0 || len(s.Filters) > 0 {
			return nil, fmt.Errorf("%s does not filter (where): it reads rollups, which keep no properties", s.Kind)
		}
	}
	q.loadExclude()
	p.ensureRollups(days(q.since, q.until))
	res := &Result{Kind: s.Kind, Window: Window{Since: q.since.Format(time.RFC3339), Until: q.until.Format(time.RFC3339)}}
	switch s.Kind {
	case "trends":
		err = q.trends(ctx, res)
	case "funnel":
		err = q.funnel(ctx, res)
	case "retention":
		err = q.retention(res)
	case "stickiness":
		err = q.stickiness(res)
	case "lifecycle":
		err = q.lifecycle(res)
	case "paths":
		err = q.paths(ctx, res)
	case "person":
		err = q.person(ctx, res)
	case "events":
		err = q.events(ctx, res)
	case "catalog":
		err = q.catalog(res)
	case "installs":
		err = q.installs(res)
	default:
		return nil, fmt.Errorf("unknown kind %q: trends, funnel, retention, stickiness, lifecycle, paths, person, events, catalog, installs", s.Kind)
	}
	if err != nil {
		return nil, err
	}
	if res.Source == "raw" || res.Source == "mixed" {
		// The same meaning as from rollups (test persons active in the window); the raw count
		// adds today's, which no rollup holds yet.
		res.ExcludedTest = max(len(q.excludedSeen), q.excludedInRollups(days(q.since, q.until)))
	}
	return res, nil
}

func (q *qctx) loadExclude() {
	q.exclude = map[string]bool{}
	if q.spec.IncludeTest {
		return
	}
	rule := map[string]any{"test": true}
	if c := q.p.Config(); c != nil {
		rule = c.ExcludeRule()
	}
	q.p.state.View(func(t *Tx) error {
		t.EachPerson(func(u string, p Person) {
			if p.matches(rule) {
				q.exclude[u] = true
			}
		})
		return nil
	})
}

// ─── Rollup reading ──────────────────────────────────────────────────────────

// DefaultRollupCache is how much decoded rollups the query cache keeps unless platform.yml
// says otherwise (analytics.rollup_cache).
const DefaultRollupCache = 256 << 20

// rollupCache keeps decoded rollups, least recently used out first, within a byte budget
// estimated by their JSON size: a busy project's day is megabytes, an idle one's kilobytes, so
// a count says nothing about memory.
type rollupCache struct {
	mu    sync.Mutex
	limit int64
	used  int64
	ll    *list.List
	m     map[string]*list.Element
}

type cachedRollup struct {
	path string
	mod  time.Time
	roll *DayRollup
	size int64
}

var rollCache = &rollupCache{limit: DefaultRollupCache, ll: list.New(), m: map[string]*list.Element{}}

// SetRollupCacheLimit sets the byte budget of the rollup cache; 0 or less is the default.
func SetRollupCacheLimit(n int64) {
	if n <= 0 {
		n = DefaultRollupCache
	}
	rollCache.mu.Lock()
	rollCache.limit = n
	rollCache.evict()
	rollCache.mu.Unlock()
}

func (c *rollupCache) get(path string, mod time.Time) *DayRollup {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	if !ok {
		return nil
	}
	cr := e.Value.(*cachedRollup)
	if !cr.mod.Equal(mod) {
		c.remove(e)
		return nil
	}
	c.ll.MoveToFront(e)
	return cr.roll
}

func (c *rollupCache) put(path string, mod time.Time, r *DayRollup, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[path]; ok {
		c.remove(e)
	}
	if size > c.limit {
		return // larger than the whole budget: read it, do not keep it
	}
	c.m[path] = c.ll.PushFront(&cachedRollup{path: path, mod: mod, roll: r, size: size})
	c.used += size
	c.evict()
}

func (c *rollupCache) evict() {
	for c.used > c.limit && c.ll.Len() > 0 {
		c.remove(c.ll.Back())
	}
}

func (c *rollupCache) remove(e *list.Element) {
	cr := e.Value.(*cachedRollup)
	c.ll.Remove(e)
	delete(c.m, cr.path)
	c.used -= cr.size
}

func (c *rollupCache) stats() (n int, used int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.used
}

// day returns the rollup of a day (cached by file time), or nil.
// ensureRollups computes the missing rollups of days in the hot window, so a query does not
// read zeros for a day the schedule has not reached yet (today, right after a start). A day
// with nothing to roll up is asked again only after 15 minutes.
func (p *Project) ensureRollups(ds []string) {
	now := p.m.now()
	// A stream younger than the retention holds everything ever received, so a day up to a
	// late week before it is computed from late events. A window trimmed by age has lost the
	// first hours of its first day and every day before it: a rollup computed from what is left
	// would count a fraction of the day and soon be frozen as final, so only whole days are.
	ws := p.WindowStart()
	first := ws.AddDate(0, 0, -lateDays).Format(DayLayout)
	if !ws.After(now.Add(-p.m.retention()).Add(time.Hour)) {
		first = ws.Truncate(24*time.Hour).AddDate(0, 0, 1).Format(DayLayout)
	}
	today := now.Format(DayLayout)
	var missing []string
	p.mu.Lock()
	for _, d := range ds {
		if d < first || d > today || now.Sub(p.noRollup[d]) < 15*time.Minute {
			continue
		}
		if _, err := os.Stat(rollupPath(p.dir, d)); err == nil {
			continue
		}
		missing = append(missing, d)
	}
	p.mu.Unlock()
	if len(missing) == 0 {
		return
	}
	p.Recompute(missing)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noRollup == nil {
		p.noRollup = map[string]time.Time{}
	}
	for _, d := range missing {
		if _, err := os.Stat(rollupPath(p.dir, d)); err != nil {
			p.noRollup[d] = now
		}
	}
}

func (p *Project) day(day string) *DayRollup {
	path := rollupPath(p.dir, day)
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if r := rollCache.get(path, fi.ModTime()); r != nil {
		return r
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw, err := decompress(b)
	if err != nil {
		return nil
	}
	var r DayRollup
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	rollCache.put(path, fi.ModTime(), &r, int64(len(raw)))
	return &r
}

// days lists the days of [from, to], inclusive.
func days(from, to time.Time) []string {
	var out []string
	for d := from.UTC().Truncate(24 * time.Hour); !d.After(to); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(DayLayout))
	}
	return out
}

func dayTime(d string) time.Time { t, _ := time.Parse(DayLayout, d); return t }

// cell is an event's count and actors in a day, optionally for one value of one dimension.
// event "any" (or "$active") is any activity.
func (q *qctx) cell(d *DayRollup, event, dim, val string) (int, *Bitmap) {
	if d == nil {
		return 0, nil
	}
	n, u := 0, newBitmap()
	for _, p := range d.Parts(q.spec.IncludeTest) {
		if isAny(event) {
			if dim == "" {
				// The count is the events that made an actor active, as raw records count it,
				// not the actors: Active has no count of its own.
				for name, e := range p.Events {
					if !passive[name] {
						n += e.N
					}
				}
				u.Or(p.Active)
			} else if c := p.ActiveBy[dim][val]; c != nil {
				n += c.N
				u.Or(c.U)
			}
			continue
		}
		e := p.Events[event]
		if e == nil {
			continue
		}
		if dim == "" {
			n += e.N
			u.Or(e.U)
		} else if c := e.By[dim][val]; c != nil {
			n += c.N
			u.Or(c.U)
		}
	}
	return n, u
}

func isAny(e string) bool { return e == "" || e == "any" || e == "$active" }

// excludedInRollups counts the excluded actors active in the range.
func (q *qctx) excludedInRollups(ds []string) int {
	if q.spec.IncludeTest {
		return 0
	}
	u := newBitmap()
	for _, d := range ds {
		if r := q.p.day(d); r != nil && r.Test != nil {
			u.Or(r.Test.Active)
		}
	}
	return u.Card()
}

// ─── Buckets ─────────────────────────────────────────────────────────────────

type bucket struct {
	label      string
	start, end time.Time // [start, end)
}

func bucketStart(t time.Time, interval string) time.Time {
	t = t.UTC()
	switch interval {
	case "hour":
		return t.Truncate(time.Hour)
	case "week":
		d := t.Truncate(24 * time.Hour)
		wd := (int(d.Weekday()) + 6) % 7 // Monday = 0
		return d.AddDate(0, 0, -wd)
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return t.Truncate(24 * time.Hour)
}

func nextBucket(t time.Time, interval string) time.Time {
	switch interval {
	case "hour":
		return t.Add(time.Hour)
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	}
	return t.AddDate(0, 0, 1)
}

func bucketLabel(t time.Time, interval string) string {
	switch interval {
	case "hour":
		return t.Format("2006-01-02T15")
	case "month":
		return t.Format("2006-01")
	}
	return t.Format(DayLayout)
}

func buckets(since, until time.Time, interval string) []bucket {
	var out []bucket
	for s := bucketStart(since, interval); s.Before(until); s = nextBucket(s, interval) {
		out = append(out, bucket{label: bucketLabel(s, interval), start: s, end: nextBucket(s, interval)})
		if len(out) > 5000 {
			break
		}
	}
	return out
}

func validInterval(i string) bool {
	return i == "hour" || i == "day" || i == "week" || i == "month"
}

// ─── Filters ─────────────────────────────────────────────────────────────────

type filter struct {
	key  string
	op   string   // = != > >= < <=
	vals []string // = and != : any of them
}

// eqOne is a plain "k=v", the only filter rollups answer.
func (f filter) eqOne() bool { return f.op == "=" && len(f.vals) == 1 }

var filterOps = []string{"!=", ">=", "<=", "=", ">", "<"}

// parseFilter reads k=v, k!=v, k>v, k>=v, k<v or k<=v; = and != take a|b (or a,b) as a list.
func parseFilter(expr string, lists bool) (filter, error) {
	at, op := -1, ""
	for i := 0; i < len(expr) && at < 0; i++ {
		for _, o := range filterOps {
			if strings.HasPrefix(expr[i:], o) {
				at, op = i, o
				break
			}
		}
	}
	if at <= 0 {
		return filter{}, fmt.Errorf("filter %q: expected k=v, k!=v, k>v, k>=v, k<v or k<=v", expr)
	}
	f := filter{key: strings.TrimSpace(expr[:at]), op: op}
	v := expr[at+len(op):]
	switch op {
	case "=", "!=":
		sep := func(r rune) bool { return r == '|' || lists && r == ',' }
		if strings.IndexFunc(v, sep) >= 0 {
			f.vals = strings.FieldsFunc(v, sep)
		} else {
			f.vals = []string{v}
		}
	default:
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return filter{}, fmt.Errorf("filter %q: %s compares numbers", expr, op)
		}
		f.vals = []string{v}
	}
	return f, nil
}

// parseFilters joins the Where map and the filter expressions: one filter per key and
// operator, so the same key with = twice means "one of".
func parseFilters(w map[string]string, exprs []string, lists bool) ([]filter, error) {
	var out []filter
	add := func(f filter) {
		if f.op == "=" || f.op == "!=" {
			for i := range out {
				if out[i].key == f.key && out[i].op == f.op {
					out[i].vals = append(out[i].vals, f.vals...)
					return
				}
			}
		}
		out = append(out, f)
	}
	for k, v := range w {
		f := filter{key: k, op: "=", vals: []string{v}}
		if strings.HasSuffix(k, "!") {
			f.key, f.op = strings.TrimSuffix(k, "!"), "!="
		}
		add(f)
	}
	for _, e := range exprs {
		f, err := parseFilter(e, lists)
		if err != nil {
			return nil, err
		}
		add(f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

func (f filter) holds(v string) bool {
	switch f.op {
	case "=":
		return slices.Contains(f.vals, v)
	case "!=":
		return !slices.Contains(f.vals, v)
	}
	x, ok := num(v)
	if v == "" || !ok {
		return false
	}
	y, _ := strconv.ParseFloat(f.vals[0], 64)
	switch f.op {
	case ">":
		return x > y
	case ">=":
		return x >= y
	case "<":
		return x < y
	}
	return x <= y
}

// breakdownKeys splits a breakdown into its keys: at most two.
func breakdownKeys(b string) ([]string, error) {
	if b == "" {
		return nil, nil
	}
	keys := strings.Split(b, ",")
	if len(keys) > 2 {
		return nil, fmt.Errorf("breakdown %q: at most two keys", b)
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("breakdown %q: an empty key", b)
		}
	}
	return keys, nil
}

// BreakdownSep joins the values of a two-key breakdown.
const BreakdownSep = " / "

// plainDim reports whether k is a dimension a rollup keeps (built-in or dm).
func plainDim(k string) bool {
	return k != "" && !strings.HasPrefix(k, "p.") && !strings.HasPrefix(k, "person.") && !strings.HasPrefix(k, "first_") && k != "event"
}

// rawCtx resolves records to actors and reads values for filters and breakdowns.
type rawCtx struct {
	q      *qctx
	t      *Tx
	actors map[string]Actor
	people map[string]Person
}

func (rc *rawCtx) actorKey(r *Record) string { return rc.t.ActorKey(r) }

// value is what a record has for a filter or breakdown key.
func (rc *rawCtx) value(r *Record, key string) string {
	switch {
	case strings.HasPrefix(key, "p."):
		return str(r.P[key[2:]])
	case strings.HasPrefix(key, "person."):
		ak := rc.actorKey(r)
		if !strings.HasPrefix(ak, "u:") {
			return ""
		}
		u := ak[2:]
		p, ok := rc.people[u]
		if !ok {
			p, _ = rc.t.Person(u)
			rc.people[u] = p
		}
		return str(p.Props[key[7:]])
	case strings.HasPrefix(key, "first_"):
		ak := rc.actorKey(r)
		a, ok := rc.actors[ak]
		if !ok {
			a, _ = rc.t.Actor(ak)
			rc.actors[ak] = a
		}
		return a.Attr(key)
	}
	return r.Dim(key)
}

func (rc *rawCtx) match(r *Record, fs []filter) bool {
	for _, f := range fs {
		if !f.holds(rc.value(r, f.key)) {
			return false
		}
	}
	return true
}

// breakdown is a record's breakdown value: one key, or two joined by BreakdownSep.
func (rc *rawCtx) breakdown(r *Record, keys []string) string {
	switch len(keys) {
	case 0:
		return ""
	case 1:
		return rc.value(r, keys[0])
	}
	return rc.value(r, keys[0]) + BreakdownSep + rc.value(r, keys[1])
}

// excluded reports (and remembers) whether the record's actor is an excluded person.
func (rc *rawCtx) excluded(r *Record) bool {
	ak := rc.actorKey(r)
	if strings.HasPrefix(ak, "u:") && rc.q.exclude[ak[2:]] {
		rc.q.excludedSeen[ak] = true
		return true
	}
	return false
}

// maxRaw bounds the raw records one query holds in memory (about 1 KB each parsed).
var maxRaw = 1_000_000

// queryTimeout bounds one query.
const queryTimeout = 2 * time.Minute

// raw reads the records with event time in [since, until] from the hot window, sets the
// result's source and partial marks, and runs fn inside one state transaction.
func (q *qctx) raw(ctx context.Context, res *Result, since, until time.Time, keep func(*Record) bool, fn func(rc *rawCtx, recs []Record) error) error {
	ws := q.p.WindowStart()
	res.Source = "raw"
	if since.Before(ws) {
		res.Partial, res.NeedsArchive = true, true
		res.Note = fmt.Sprintf("raw records before %s are only in the archive: counted from %s; read the rest with DuckDB over the archive URLs", ws.Format(DayLayout), ws.Format(DayLayout))
		if urls, err := q.p.ArchiveURLs(ctx, since, ws, time.Hour, false); err == nil {
			res.Archive = urls
		}
		since = ws
	}
	recs, err := q.p.readRecordsCtx(ctx, since.Add(-time.Hour), time.Time{}, func(r *Record) bool {
		return !r.t.Before(since) && !r.t.After(until) && (keep == nil || keep(r))
	}, maxRaw)
	if err != nil {
		return err
	}
	// Dedup by id, in event order.
	seen := make(map[string]bool, len(recs))
	out := recs[:0]
	for _, r := range recs {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].t.Before(out[j].t) })
	return q.p.state.View(func(t *Tx) error {
		return fn(&rawCtx{q: q, t: t, actors: map[string]Actor{}, people: map[string]Person{}}, out)
	})
}

// ─── Trends ──────────────────────────────────────────────────────────────────

var rollupMath = map[string]bool{"total": true, "uniques": true, "dau": true, "wau": true, "mau": true, "per_user_avg": true}

func (q *qctx) trends(ctx context.Context, res *Result) error {
	s := q.spec
	if len(s.Events) == 0 {
		return errors.New("trends: name at least one event (any = any activity)")
	}
	if s.Math == "" {
		s.Math = "total"
	}
	if s.Interval == "" {
		s.Interval = "day"
	}
	if !validInterval(s.Interval) {
		return fmt.Errorf("interval %q: hour, day, week or month", s.Interval)
	}
	switch s.Math {
	case "total", "uniques", "dau", "wau", "mau", "per_user_avg":
	case "sum", "avg", "p50", "p75", "p90", "p95", "p99":
		if s.Prop == "" {
			return fmt.Errorf("math %s needs --prop", s.Math)
		}
	default:
		return fmt.Errorf("math %q: total, uniques, dau, wau, mau, per_user_avg, sum, avg, p50, p75, p90, p95, p99", s.Math)
	}
	q.spec = s
	fs, err := parseFilters(s.Where, s.Filters, true)
	if err != nil {
		return err
	}
	brKeys, err := breakdownKeys(s.Breakdown)
	if err != nil {
		return err
	}
	useRollup := rollupMath[s.Math] && s.Interval != "hour" && len(fs) <= 1 && len(brKeys) <= 1
	for _, f := range fs {
		useRollup = useRollup && plainDim(f.key) && f.eqOne() && (s.Breakdown == "" || s.Breakdown == f.key)
	}
	if s.Breakdown != "" && !plainDim(s.Breakdown) {
		useRollup = false
	}
	run := func(since, until time.Time) ([]Series, error) {
		if useRollup {
			return q.trendsRollup(res, since, until, fs)
		}
		var out []Series
		err := q.trendsRaw(ctx, res, since, until, fs, brKeys, &out)
		return out, err
	}
	series, err := run(q.since, q.until)
	if err != nil {
		return err
	}
	if s.Compare {
		span := q.until.Sub(q.since)
		prev, err := run(q.since.Add(-span), q.since)
		if err != nil {
			return err
		}
		byKey := map[string]Series{}
		for _, p := range prev {
			byKey[p.Event+"\x00"+p.Breakdown] = p
		}
		for i := range series {
			if p, ok := byKey[series[i].Event+"\x00"+series[i].Breakdown]; ok {
				series[i].Previous = p.Points
			}
		}
	}
	res.Series = series
	return nil
}

func (q *qctx) trendsRollup(res *Result, since, until time.Time, fs []filter) ([]Series, error) {
	s := q.spec
	res.Source = "rollup"
	bs := buckets(since, until, s.Interval)
	if len(bs) == 0 {
		return nil, nil
	}
	lookback := 0
	switch s.Math {
	case "wau":
		lookback = 6
	case "mau":
		lookback = 29
	}
	first := bs[0].start.AddDate(0, 0, -lookback)
	ds := days(first, until)
	rolls := map[string]*DayRollup{}
	for _, d := range ds {
		rolls[d] = q.p.day(d)
	}
	res.ExcludedTest = q.excludedInRollups(days(since, until))
	today := q.now.Format(DayLayout)
	var whereDim, whereVal string
	if len(fs) == 1 {
		whereDim, whereVal = fs[0].key, fs[0].vals[0]
	}
	var out []Series
	for _, ev := range s.Events {
		// Breakdown values: every value seen in the range, the 20 largest.
		vals := []string{""}
		if s.Breakdown != "" && whereDim == "" {
			tot := map[string]int{}
			for _, d := range days(since, until) {
				for _, p := range partsOf(rolls[d], s.IncludeTest) {
					var m map[string]*Cell
					if isAny(ev) {
						m = p.ActiveBy[s.Breakdown]
					} else if e := p.Events[ev]; e != nil {
						m = e.By[s.Breakdown]
					}
					for v, c := range m {
						tot[v] += c.N
					}
				}
			}
			vals = topKeys(tot, 20)
		}
		for _, v := range vals {
			lookDim, lookVal := "", ""
			switch {
			case whereDim != "":
				lookDim, lookVal = whereDim, whereVal
			case s.Breakdown != "" && v != "":
				lookDim, lookVal = s.Breakdown, v
			}
			cellOf := func(d string) (int, *Bitmap) {
				r, ok := rolls[d]
				if !ok {
					r = q.p.day(d)
				}
				return q.cell(r, ev, lookDim, lookVal)
			}
			// rolling is the actors of the lookback window that ends on end (wau, mau).
			rolling := func(end time.Time) *Bitmap {
				u := newBitmap()
				for _, d := range days(end.AddDate(0, 0, -lookback), end) {
					_, b := cellOf(d)
					u.Or(b)
				}
				return u
			}
			ser := Series{Event: ev, Breakdown: v}
			var allDays []string // every day of the range the buckets counted, for the total
			for _, b := range bs {
				var inRange []string
				for _, d := range days(b.start, b.end.Add(-time.Nanosecond)) {
					if d <= today && !dayTime(d).Before(since.Truncate(24*time.Hour)) && !dayTime(d).After(until) {
						inRange = append(inRange, d)
					}
				}
				if len(inRange) == 0 {
					continue
				}
				allDays = append(allDays, inRange...)
				var val float64
				switch s.Math {
				case "total":
					for _, d := range inRange {
						n, _ := cellOf(d)
						val += float64(n)
					}
				case "uniques", "per_user_avg":
					u, n := newBitmap(), 0
					for _, d := range inRange {
						c, b := cellOf(d)
						n += c
						u.Or(b)
					}
					val = float64(u.Card())
					if s.Math == "per_user_avg" {
						val = 0
						if u.Card() > 0 {
							val = round2(float64(n) / float64(u.Card()))
						}
					}
				case "dau":
					sum := 0
					for _, d := range inRange {
						_, b := cellOf(d)
						sum += b.Card()
					}
					val = round2(float64(sum) / float64(len(inRange)))
				case "wau", "mau":
					val = float64(rolling(dayTime(inRange[len(inRange)-1])).Card())
				}
				ser.Points = append(ser.Points, Point{T: b.label, V: val})
				if s.Math == "total" {
					ser.Total += val
				}
			}
			if s.Math != "total" && len(allDays) > 0 {
				ser.Total = rollupTotal(s.Math, allDays, cellOf, rolling)
			}
			ser.TotalKind = totalKind(s.Math)
			out = append(out, ser)
		}
	}
	return out, nil
}

// rollupTotal is the whole-range value of a series whose math does not add up across buckets:
// uniques counts the distinct actors of every day of the range, per_user_avg divides all the
// events by them, and dau, wau and mau are the average of the daily value over the days.
func rollupTotal(m string, ds []string, cellOf func(string) (int, *Bitmap), rolling func(time.Time) *Bitmap) float64 {
	switch m {
	case "uniques", "per_user_avg":
		u, n := newBitmap(), 0
		for _, d := range ds {
			c, b := cellOf(d)
			n += c
			u.Or(b)
		}
		if m == "uniques" {
			return float64(u.Card())
		}
		if u.Card() == 0 {
			return 0
		}
		return round2(float64(n) / float64(u.Card()))
	case "dau", "wau", "mau":
		sum := 0
		for _, d := range ds {
			if m == "dau" {
				_, b := cellOf(d)
				sum += b.Card()
			} else {
				sum += rolling(dayTime(d)).Card()
			}
		}
		return round2(float64(sum) / float64(len(ds)))
	}
	return 0
}

func partsOf(d *DayRollup, includeTest bool) []*Part {
	if d == nil {
		return nil
	}
	return d.Parts(includeTest)
}

func topKeys(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func (q *qctx) trendsRaw(ctx context.Context, res *Result, since, until time.Time, fs []filter, brKeys []string, out *[]Series) error {
	s := q.spec
	want := map[string]bool{}
	anyEv := false
	for _, e := range s.Events {
		want[e] = true
		anyEv = anyEv || isAny(e)
	}
	lookback := map[string]int{"wau": 6, "mau": 29}[s.Math]
	readSince := since
	if lookback > 0 {
		readSince = bucketStart(since, s.Interval).AddDate(0, 0, -lookback)
	}
	bs := buckets(since, until, s.Interval)
	return q.raw(ctx, res, readSince, until, func(r *Record) bool { return want[r.E] || anyEv && !passive[r.E] }, func(rc *rawCtx, recs []Record) error {
		type acc struct {
			n      int
			actors map[string]bool
			vals   []float64
		}
		type key struct{ ev, br, b string }
		accs := map[key]*acc{}
		daily := map[key]map[string]bool{} // (ev, br, day) → actors, for dau/wau/mau
		brTotals := map[string]map[string]int{}
		for i := range recs {
			r := &recs[i]
			if rc.excluded(r) || !rc.match(r, fs) {
				continue
			}
			ak := rc.actorKey(r)
			for _, ev := range s.Events {
				if !(ev == r.E || isAny(ev) && !passive[r.E]) {
					continue
				}
				br := rc.breakdown(r, brKeys)
				if brTotals[ev] == nil {
					brTotals[ev] = map[string]int{}
				}
				dk := key{ev, br, r.Day()}
				if daily[dk] == nil {
					daily[dk] = map[string]bool{}
				}
				daily[dk][ak] = true
				if r.t.Before(since) {
					continue
				}
				brTotals[ev][br]++
				k := key{ev, br, bucketLabel(bucketStart(r.t, s.Interval), s.Interval)}
				a := accs[k]
				if a == nil {
					a = &acc{actors: map[string]bool{}}
					accs[k] = a
				}
				a.n++
				a.actors[ak] = true
				if s.Prop != "" {
					if v, ok := num(r.P[s.Prop]); ok {
						a.vals = append(a.vals, v)
					}
				}
			}
		}
		today := q.now.Format(DayLayout)
		for _, ev := range s.Events {
			brs := []string{""}
			if s.Breakdown != "" {
				brs = topKeys(brTotals[ev], 20)
			}
			for _, br := range brs {
				ser := Series{Event: ev, Breakdown: br}
				var all []float64
				events, actors := 0, map[string]bool{} // the whole range, for the total
				for _, b := range bs {
					a := accs[key{ev, br, b.label}]
					if a == nil {
						a = &acc{actors: map[string]bool{}}
					}
					events += a.n
					for ak := range a.actors {
						actors[ak] = true
					}
					var v float64
					switch s.Math {
					case "total":
						v = float64(a.n)
					case "uniques":
						v = float64(len(a.actors))
					case "per_user_avg":
						if len(a.actors) > 0 {
							v = round2(float64(a.n) / float64(len(a.actors)))
						}
					case "dau":
						n, sum := 0, 0
						for d := b.start; d.Before(b.end) && d.Format(DayLayout) <= today; d = d.AddDate(0, 0, 1) {
							if s.Interval == "hour" {
								break
							}
							if d.Before(since.Truncate(24*time.Hour)) || d.After(until) {
								continue // a day of the bucket outside the range, as in rollups
							}
							n++
							sum += len(daily[key{ev, br, d.Format(DayLayout)}])
						}
						if s.Interval == "hour" {
							n, sum = 1, len(a.actors)
						}
						if n > 0 {
							v = round2(float64(sum) / float64(n))
						}
					case "wau", "mau":
						end := b.end.Add(-time.Nanosecond)
						if end.After(q.now) {
							end = q.now
						}
						u := map[string]bool{}
						for _, d := range days(end.AddDate(0, 0, -lookback), end) {
							for ak := range daily[key{ev, br, d}] {
								u[ak] = true
							}
						}
						v = float64(len(u))
					case "sum", "avg", "p50", "p75", "p90", "p95", "p99":
						all = append(all, a.vals...)
						v = aggregate(s.Math, a.vals)
					}
					ser.Points = append(ser.Points, Point{T: b.label, V: v})
				}
				switch s.Math {
				case "total":
					for _, p := range ser.Points {
						ser.Total += p.V
					}
				case "sum", "avg", "p50", "p75", "p90", "p95", "p99":
					ser.Total = aggregate(s.Math, all)
				case "uniques":
					ser.Total = float64(len(actors))
				case "per_user_avg":
					if len(actors) > 0 {
						ser.Total = round2(float64(events) / float64(len(actors)))
					}
				case "dau", "wau", "mau":
					// The average of the daily value over the days of the range, as in rollups.
					n, sum := 0, 0
					for _, d := range days(since, until) {
						if d > today {
							break
						}
						u := map[string]bool{}
						for _, w := range days(dayTime(d).AddDate(0, 0, -lookback), dayTime(d)) {
							for ak := range daily[key{ev, br, w}] {
								u[ak] = true
							}
						}
						n++
						sum += len(u)
					}
					if n > 0 {
						ser.Total = round2(float64(sum) / float64(n))
					}
				}
				ser.TotalKind = totalKind(s.Math)
				*out = append(*out, ser)
			}
		}
		return nil
	})
}

// totalKind says what Series.Total is when it is not a count over the range.
func totalKind(m string) string {
	switch m {
	case "dau", "wau", "mau":
		return "average"
	}
	return ""
}

func aggregate(m string, vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	switch m {
	case "sum", "avg":
		t := 0.0
		for _, v := range vals {
			t += v
		}
		if m == "avg" {
			return round2(t / float64(len(vals)))
		}
		return round2(t)
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	p := 0.5 // nearest rank
	if n, err := strconv.Atoi(strings.TrimPrefix(m, "p")); err == nil && n > 0 && n < 100 {
		p = float64(n) / 100
	}
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// ─── Funnel ──────────────────────────────────────────────────────────────────

type step struct {
	label string
	event string
	fs    []filter
}

// parseStep reads a funnel step: "name" or "name:k=v,k>v,..." (conditions joined by commas,
// so a list of values is written a|b).
func parseStep(s string) (step, error) {
	st := step{label: s, event: s}
	if ev, cond, ok := strings.Cut(s, ":"); ok {
		st.event = ev
		fs, err := parseFilters(nil, strings.Split(cond, ","), false)
		if err != nil {
			return st, fmt.Errorf("step %q: %v", s, err)
		}
		st.fs = fs
	}
	return st, nil
}

func (q *qctx) funnel(ctx context.Context, res *Result) error {
	s := q.spec
	if len(s.Events) < 2 {
		return errors.New("funnel: at least two steps")
	}
	window, err := parseDur(s.Window, 14*24*time.Hour)
	if err != nil {
		return fmt.Errorf("window: %v", err)
	}
	order := s.Order
	if order == "" {
		order = "ordered"
	}
	if order != "ordered" && order != "strict" && order != "any" {
		return fmt.Errorf("order %q: ordered, strict or any", order)
	}
	steps := make([]step, len(s.Events))
	want := map[string]bool{}
	for i, e := range s.Events {
		if steps[i], err = parseStep(e); err != nil {
			return err
		}
		want[steps[i].event] = true
	}
	global, err := parseFilters(s.Where, s.Filters, true)
	if err != nil {
		return err
	}
	brKeys, err := breakdownKeys(s.Breakdown)
	if err != nil {
		return err
	}
	until := q.until.Add(window)
	if until.After(q.now) {
		until = q.now
	}
	keep := func(r *Record) bool { return order == "strict" || want[r.E] }
	return q.raw(ctx, res, q.since, until, keep, func(rc *rawCtx, recs []Record) error {
		byActor := map[string][]*Record{}
		var order0 []string
		for i := range recs {
			r := &recs[i]
			if rc.excluded(r) {
				continue
			}
			ak := rc.actorKey(r)
			if ak == "" {
				continue
			}
			if _, ok := byActor[ak]; !ok {
				order0 = append(order0, ak)
			}
			byActor[ak] = append(byActor[ak], r)
		}
		type outcome struct {
			reached int
			times   []time.Time
			br      string
		}
		matches := func(r *Record, st step) bool { return r.E == st.event && rc.match(r, st.fs) && rc.match(r, global) }
		var outs []outcome
		for _, ak := range order0 {
			evs := byActor[ak]
			best := outcome{}
			for i, r := range evs {
				if !matches(r, steps[0]) || r.t.Before(q.since) || r.t.After(q.until) {
					continue
				}
				o := outcome{reached: 1, times: []time.Time{r.t}}
				o.br = rc.breakdown(r, brKeys)
				deadline := r.t.Add(window)
				switch order {
				case "ordered":
					k := 1
					for _, r2 := range evs[i+1:] {
						if k >= len(steps) || r2.t.After(deadline) {
							break
						}
						if matches(r2, steps[k]) {
							o.times = append(o.times, r2.t)
							k++
						}
					}
					o.reached = k
				case "strict":
					k := 1
					for _, r2 := range evs[i+1:] {
						if k >= len(steps) || r2.t.After(deadline) {
							break
						}
						if matches(r2, steps[k]) {
							o.times = append(o.times, r2.t)
							k++
						} else {
							break
						}
					}
					o.reached = k
				case "any":
					got := map[int]time.Time{0: r.t}
					for _, r2 := range evs[i+1:] {
						if r2.t.After(deadline) {
							break
						}
						for k := 1; k < len(steps); k++ {
							if _, ok := got[k]; !ok && matches(r2, steps[k]) {
								got[k] = r2.t
								break
							}
						}
					}
					k := 1
					for ; k < len(steps); k++ {
						if _, ok := got[k]; !ok {
							break
						}
					}
					o.reached = k
					for j := 1; j < k; j++ {
						o.times = append(o.times, got[j])
					}
				}
				if o.reached > best.reached {
					best = o
				}
				if best.reached == len(steps) {
					break
				}
			}
			if best.reached > 0 {
				outs = append(outs, best)
			}
		}
		groups := map[string][]outcome{}
		tot := map[string]int{}
		for _, o := range outs {
			groups[o.br] = append(groups[o.br], o)
			tot[o.br]++
		}
		brs := []string{""}
		if s.Breakdown != "" {
			brs = topKeys(tot, 10)
		}
		for _, br := range brs {
			os := groups[br]
			f := Funnel{Breakdown: br}
			for k, st := range steps {
				n := 0
				var deltas []float64
				for _, o := range os {
					if o.reached > k {
						n++
						if k > 0 && len(o.times) > k {
							deltas = append(deltas, o.times[k].Sub(o.times[k-1]).Seconds())
						}
					}
				}
				fsStep := FunnelStep{Step: st.label, Count: n}
				if k > 0 {
					prev := f.Steps[k-1].Count
					if prev > 0 {
						v := round4(float64(n) / float64(prev))
						fsStep.FromPrev = &v
					}
					if first := f.Steps[0].Count; first > 0 {
						v := round4(float64(n) / float64(first))
						fsStep.FromStart = &v
					}
					if len(deltas) > 0 {
						m := aggregate("p50", deltas)
						fsStep.MedianSeconds = &m
					}
				}
				f.Steps = append(f.Steps, fsStep)
			}
			res.Funnels = append(res.Funnels, f)
		}
		return nil
	})
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// ─── Retention ───────────────────────────────────────────────────────────────

// periodSet is the actors of an event (or of "first"/"any") over a period of days.
func (q *qctx) periodSet(event string, from, to time.Time) *Bitmap {
	u := newBitmap()
	today := q.now.Format(DayLayout)
	for _, d := range days(from, to) {
		if d > today {
			break
		}
		r := q.p.day(d)
		if r == nil {
			continue
		}
		if event == "first" {
			for _, p := range r.Parts(q.spec.IncludeTest) {
				u.Or(p.First)
			}
			continue
		}
		_, b := q.cell(r, event, "", "")
		u.Or(b)
	}
	return u
}

// actorAttr returns an actor attribute by number, cached.
func (q *qctx) actorAttr(t *Tx, cache map[uint32]Actor, id uint32, attr string) string {
	a, ok := cache[id]
	if !ok {
		a, _ = t.ActorByID(id)
		cache[id] = a
	}
	return a.Attr(attr)
}

func (q *qctx) retention(res *Result) error {
	s := q.spec
	res.Source = "rollup"
	period := s.Period
	if period == "" {
		period = "week"
	}
	if period != "day" && period != "week" && period != "month" {
		return fmt.Errorf("period %q: day, week or month", period)
	}
	start, ret := s.Start, s.Return
	if start == "" {
		start = "first"
	}
	if ret == "" {
		ret = "any"
	}
	brKeys, err := breakdownKeys(s.Breakdown)
	if err != nil {
		return err
	}
	for _, k := range brKeys {
		if !strings.HasPrefix(k, "first_") {
			return errors.New("retention breaks down by actor attributes: first_source, first_campaign, first_pf, first_app or first_cc")
		}
	}
	bs := buckets(q.since, q.until, period)
	res.ExcludedTest = q.excludedInRollups(days(q.since, q.until))
	cache := map[uint32]Actor{}
	return q.p.state.View(func(t *Tx) error {
		for _, b := range bs {
			cohort := q.periodSet(start, b.start, b.end.Add(-time.Nanosecond))
			groups := map[string]*Bitmap{"": cohort}
			if s.Breakdown != "" {
				groups = map[string]*Bitmap{}
				cohort.Each(func(id uint32) {
					v := q.actorAttr(t, cache, id, brKeys[0])
					if len(brKeys) == 2 {
						v += BreakdownSep + q.actorAttr(t, cache, id, brKeys[1])
					}
					if groups[v] == nil {
						groups[v] = newBitmap()
					}
					groups[v].Add(id)
				})
			}
			keys := make([]string, 0, len(groups))
			for k := range groups {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				g := groups[k]
				c := Cohort{Start: b.label, Breakdown: k, Size: g.Card()}
				for ps := b.start; !ps.After(q.now) && len(c.Values) < 400; ps = nextBucket(ps, period) {
					if ps.Equal(b.start) {
						c.Values = append(c.Values, g.Card())
						continue
					}
					c.Values = append(c.Values, and(g, q.periodSet(ret, ps, nextBucket(ps, period).Add(-time.Nanosecond))).Card())
				}
				res.Cohorts = append(res.Cohorts, c)
			}
		}
		return nil
	})
}

// ─── Stickiness ──────────────────────────────────────────────────────────────

func (q *qctx) stickiness(res *Result) error {
	ev := q.spec.Event
	if ev == "" && len(q.spec.Events) > 0 {
		ev = q.spec.Events[0]
	}
	res.Source = "rollup"
	ds := days(q.since, q.until)
	res.ExcludedTest = q.excludedInRollups(ds)
	count := map[uint32]int{}
	for _, d := range ds {
		_, b := q.cell(q.p.day(d), ev, "", "")
		b.Each(func(id uint32) { count[id]++ })
	}
	hist := map[int]int{}
	for _, n := range count {
		hist[n]++
	}
	res.Columns = []string{"days", "actors", "share"}
	for n := 1; n <= len(ds); n++ {
		if hist[n] == 0 {
			continue
		}
		res.Rows = append(res.Rows, Row{"days": n, "actors": hist[n], "share": round4(float64(hist[n]) / float64(len(count)))})
	}
	return nil
}

// ─── Lifecycle ───────────────────────────────────────────────────────────────

func (q *qctx) lifecycle(res *Result) error {
	ev := q.spec.Event
	if ev == "" && len(q.spec.Events) > 0 {
		ev = q.spec.Events[0]
	}
	interval := q.spec.Interval
	if interval == "" {
		interval = "week"
	}
	if interval != "day" && interval != "week" && interval != "month" {
		return fmt.Errorf("interval %q: day, week or month", interval)
	}
	res.Source = "rollup"
	bs := buckets(q.since, q.until, interval)
	if len(bs) == 0 {
		return nil
	}
	res.ExcludedTest = q.excludedInRollups(days(q.since, q.until))
	prevStart := bs[0].start.AddDate(0, 0, -1)
	prev := q.periodSet(ev, bucketStart(prevStart, interval), bs[0].start.Add(-time.Nanosecond))
	res.Columns = []string{"t", "new", "returning", "resurrecting", "dormant"}
	for _, b := range bs {
		end := b.end.Add(-time.Nanosecond)
		cur := q.periodSet(ev, b.start, end)
		first := q.periodSet("first", b.start, end)
		newA := and(cur, first)
		returning := and(cur, prev)
		returning.AndNot(newA)
		resurrecting := cur.Clone()
		resurrecting.AndNot(prev)
		resurrecting.AndNot(newA)
		dormant := prev.Clone()
		dormant.AndNot(cur)
		res.Rows = append(res.Rows, Row{"t": b.label, "new": newA.Card(), "returning": returning.Card(), "resurrecting": resurrecting.Card(), "dormant": -dormant.Card()})
		prev = cur
	}
	return nil
}

// ─── Paths ───────────────────────────────────────────────────────────────────

func (q *qctx) paths(ctx context.Context, res *Result) error {
	s := q.spec
	depth := s.Depth
	if depth == 0 {
		depth = 3
	}
	if depth < 3 || depth > 5 {
		return fmt.Errorf("depth %d: 3 to 5 screens", s.Depth)
	}
	limit := s.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		return fmt.Errorf("limit %d: at most 1000", limit)
	}
	fs, err := parseFilters(s.Where, s.Filters, true)
	if err != nil {
		return err
	}
	return q.raw(ctx, res, q.since, q.until, func(r *Record) bool { return r.E == EventPageview }, func(rc *rawCtx, recs []Record) error {
		seqs := map[string][]string{}
		var order []string
		views, actors := map[string]int{}, map[string]map[string]bool{}
		for i := range recs {
			r := &recs[i]
			if rc.excluded(r) || !rc.match(r, fs) {
				continue
			}
			screen := r.Dim("screen_name")
			if screen == "" {
				continue
			}
			views[screen]++
			if actors[screen] == nil {
				actors[screen] = map[string]bool{}
			}
			ak := rc.actorKey(r)
			actors[screen][ak] = true
			sk := r.S
			if sk == "" {
				sk = ak + "/" + r.Day()
			}
			if _, ok := seqs[sk]; !ok {
				order = append(order, sk)
			}
			if sq := seqs[sk]; len(sq) == 0 || sq[len(sq)-1] != screen {
				seqs[sk] = append(sq, screen)
			}
		}
		counts := map[string]int{}
		for _, sk := range order {
			sq := seqs[sk]
			switch {
			case s.From != "":
				for i, sc := range sq {
					if end := min(i+depth, len(sq)); sc == s.From && end-i >= 2 {
						counts[strings.Join(sq[i:end], " → ")]++
					}
				}
			case s.To != "":
				for i, sc := range sq {
					if start := max(i-depth+1, 0); sc == s.To && i-start >= 1 {
						counts[strings.Join(sq[start:i+1], " → ")]++
					}
				}
			default:
				for i := 0; i+1 < len(sq); i++ {
					counts[sq[i]+" → "+sq[i+1]]++
				}
			}
		}
		res.Columns = []string{"path", "count"}
		for _, k := range topKeys(counts, limit) {
			res.Rows = append(res.Rows, Row{"path": k, "count": counts[k]})
		}
		// The top screens are a table of their own.
		res.ScreenColumns = []string{"screen", "views", "actors"}
		for _, k := range topKeys(views, limit) {
			res.Screens = append(res.Screens, Row{"screen": k, "views": views[k], "actors": len(actors[k])})
		}
		return nil
	})
}

// ─── Person and events ───────────────────────────────────────────────────────

func (q *qctx) person(ctx context.Context, res *Result) error {
	u := q.spec.User
	if u == "" {
		return errors.New("person: the user id is required")
	}
	pr := &PersonResult{User: u}
	q.p.state.View(func(t *Tx) error {
		if p, ok := t.Person(u); ok {
			pr.Props, pr.FirstTS, pr.LastTS, pr.Devices = p.Props, p.FirstTS, p.LastTS, p.Devices
		}
		if a, ok := t.Actor("u:" + u); ok {
			pr.Actor = &a
		}
		return nil
	})
	res.Person = pr
	limit := q.spec.Limit
	if limit <= 0 {
		limit = 100
	}
	q.spec.IncludeTest = true // a person asked for by id is shown even when excluded
	return q.raw(ctx, res, q.since, q.until, q.userRecords(u), func(rc *rawCtx, recs []Record) error {
		var lines []json.RawMessage
		for i := range recs {
			r := &recs[i]
			if rc.actorKey(r) == "u:"+u {
				b, _ := json.Marshal(r)
				lines = append(lines, b)
			}
		}
		if len(lines) > limit {
			lines = lines[len(lines)-limit:]
		}
		pr.Events = lines
		return nil
	})
}

// userRecords keeps the records that may belong to a user before they are parsed into memory:
// the user's own, and those of the devices linked to the user (anonymous events count for the
// person). The exact check (the actor) runs after.
func (q *qctx) userRecords(u string) func(*Record) bool {
	devices := map[string]bool{}
	q.p.state.View(func(t *Tx) error {
		if p, ok := t.Person(u); ok {
			for _, d := range p.Devices {
				devices[d] = true
			}
		}
		t.tx.Bucket(bLinks).ForEach(func(k, v []byte) error {
			if string(v) == u {
				devices[string(k)] = true
			}
			return nil
		})
		return nil
	})
	return func(r *Record) bool { return r.User() == u || r.D != "" && devices[r.D] }
}

func (q *qctx) events(ctx context.Context, res *Result) error {
	s := q.spec
	limit := s.Limit
	if limit <= 0 {
		limit = 50
	}
	fs, err := parseFilters(s.Where, s.Filters, true)
	if err != nil {
		return err
	}
	byUser := func(*Record) bool { return true }
	if s.User != "" {
		byUser = q.userRecords(s.User)
	}
	keep := func(r *Record) bool {
		return (s.Event == "" || r.E == s.Event) && (s.Device == "" || r.D == s.Device) && byUser(r)
	}
	return q.raw(ctx, res, q.since, q.until, keep, func(rc *rawCtx, recs []Record) error {
		var out []json.RawMessage
		for i := range recs {
			r := &recs[i]
			if s.User != "" && rc.actorKey(r) != "u:"+s.User {
				continue
			}
			if s.Device != "" && r.D != s.Device {
				continue
			}
			if s.User == "" && rc.excluded(r) {
				continue
			}
			if !rc.match(r, fs) {
				continue
			}
			b, _ := json.Marshal(r)
			out = append(out, b)
		}
		if len(out) > limit {
			out = out[len(out)-limit:]
		}
		res.Events = out
		return nil
	})
}

// ─── Catalog and installs ────────────────────────────────────────────────────

func (q *qctx) catalog(res *Result) error {
	res.Source = "rollup"
	var cat Catalog
	q.p.state.GetMeta("catalog", &cat)
	n7, n30 := map[string]int{}, map[string]int{}
	d7 := q.now.AddDate(0, 0, -6).Format(DayLayout)
	for _, d := range days(q.now.AddDate(0, 0, -29), q.now) {
		r := q.p.day(d)
		if r == nil {
			continue
		}
		for _, p := range r.Parts(true) {
			for e, agg := range p.Events {
				n30[e] += agg.N
				if d >= d7 {
					n7[e] += agg.N
				}
			}
		}
	}
	res.Columns = []string{"event", "last", "n7", "n30", "src", "dm", "p", "set"}
	for _, name := range cat.Names() {
		e := cat.Events[name]
		res.Rows = append(res.Rows, Row{"event": name, "last": e.Last, "n7": n7[name], "n30": n30[name],
			"src": keysOf(e.Src), "dm": keysOf(e.DM), "p": keysOf(e.P), "set": keysOf(e.Set), "apps": keysOf(e.Apps)})
	}
	return nil
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (q *qctx) installs(res *Result) error {
	res.Source = "rollup"
	by := q.spec.Breakdown
	if by == "" {
		by = "source"
	}
	ok := false
	for _, d := range InstallDims {
		ok = ok || d == by
	}
	if !ok {
		return fmt.Errorf("installs break down by %s", strings.Join(InstallDims, ", "))
	}
	ds := days(q.since, q.until)
	res.ExcludedTest = q.excludedInRollups(ds)
	type acc struct {
		install, reinstall, firstVisit int
		base, ret                      [3]int // D1, D7, D30
	}
	accs := map[string]*acc{}
	get := func(k string) *acc {
		if accs[k] == nil {
			accs[k] = &acc{}
		}
		return accs[k]
	}
	suspect := 0
	today := q.now.Format(DayLayout)
	offsets := [3]int{1, 7, 30}
	for _, d := range ds {
		r := q.p.day(d)
		if r == nil {
			continue
		}
		for _, p := range r.Parts(q.spec.IncludeTest) {
			if p.Installs == nil {
				continue
			}
			suspect += p.Installs.Suspect
			for _, kind := range []string{KindInstall, KindReinstall, KindFirstVisit} {
				agg := p.Installs.Get(kind)
				for v, c := range agg.By[by] {
					a := get(v)
					switch kind {
					case KindInstall:
						a.install += c.N
					case KindReinstall:
						a.reinstall += c.N
					default:
						a.firstVisit += c.N
					}
					for i, off := range offsets {
						rd := dayTime(d).AddDate(0, 0, off).Format(DayLayout)
						if rd >= today {
							continue // the return day is not over yet
						}
						_, active := q.cell(q.p.day(rd), "any", "", "")
						a.base[i] += c.U.Card()
						a.ret[i] += and(c.U, active).Card()
					}
				}
			}
		}
	}
	res.Columns = []string{by, "install", "reinstall", "first_visit", "d1", "d7", "d30"}
	tot := map[string]int{}
	for k, a := range accs {
		tot[k] = a.install + a.reinstall + a.firstVisit
	}
	for _, k := range topKeys(tot, 50) {
		a := accs[k]
		row := Row{by: k, "install": a.install, "reinstall": a.reinstall, "first_visit": a.firstVisit}
		for i, name := range []string{"d1", "d7", "d30"} {
			if a.base[i] > 0 {
				row[name] = round4(float64(a.ret[i]) / float64(a.base[i]))
				row[name+"_base"] = a.base[i]
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if suspect > 0 {
		res.Note = fmt.Sprintf("%d suspicious $first_open not counted", suspect)
	}
	return nil
}
