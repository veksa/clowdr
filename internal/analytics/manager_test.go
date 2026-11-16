package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
)

type fakeSender struct {
	mu   sync.Mutex
	msgs []string
	fail error
}

func (f *fakeSender) Deliver(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.msgs = append(f.msgs, text)
	return nil
}

func (f *fakeSender) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

type env struct {
	t    *testing.T
	m    *Manager
	ls   *logs.Store
	now  time.Time
	tg   *fakeSender
	tok  string
	chat string
}

func newEnv(t *testing.T, now time.Time) *env {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ls := &logs.Store{BufDir: filepath.Join(dir, "buf"), DataDir: filepath.Join(dir, "logs"), Log: log}
	if err := ls.Open(); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ls: ls, now: now, tg: &fakeSender{}}
	e.m = &Manager{Dir: filepath.Join(dir, "analytics"), Logs: ls, Log: log, Now: func() time.Time { return e.now },
		Telegram: func(token, chat string) Sender { e.tok, e.chat = token, chat; return e.tg }}
	t.Cleanup(func() { e.m.Close(); ls.Close() })
	return e
}

func (e *env) project() *Project {
	p, err := e.m.Project("shop")
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// feed stores lines in the shop's analytics stream as if received at the record time plus a
// second (or at `at` when it is set), and hands them to the manager like the collector does.
func (e *env) feed(at time.Time, lines ...string) {
	e.t.Helper()
	k := logs.Key{Project: "shop", Kind: logs.KindAnalytics, Name: "server"}
	for _, l := range lines {
		recv := at
		if recv.IsZero() {
			if r, err := Parse(l); err == nil {
				recv = r.Time().Add(time.Second)
			} else {
				recv = e.now
			}
		}
		line := logs.Line{T: recv.UnixNano(), L: l}
		if err := e.ls.Append(k, line); err != nil {
			e.t.Fatal(err)
		}
		e.m.OnLine(k, line)
	}
	e.project().Sync()
}

// feedAt is feed for many lines with their own receive times, synced once.
func (e *env) feedAt(lines []string, recv []time.Time) {
	e.t.Helper()
	k := logs.Key{Project: "shop", Kind: logs.KindAnalytics, Name: "server"}
	for i, l := range lines {
		line := logs.Line{T: recv[i].UnixNano(), L: l}
		if err := e.ls.Append(k, line); err != nil {
			e.t.Fatal(err)
		}
		e.m.OnLine(k, line)
	}
	e.project().Sync()
}

func (e *env) configure(c config.AnalyticsConf, secrets map[string]string) {
	e.t.Helper()
	p := e.project()
	if _, err := p.SetConfig(c); err != nil {
		e.t.Fatal(err)
	}
	for k, v := range secrets {
		if err := p.SetSecret(k, v); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) query(s Spec) *Result {
	e.t.Helper()
	res, err := e.project().Query(context.Background(), s)
	if err != nil {
		e.t.Fatalf("%+v: %v", s, err)
	}
	return res
}

func points(s Series) []float64 {
	var out []float64
	for _, p := range s.Points {
		out = append(out, p.V)
	}
	return out
}

func eq(a, b []float64) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func fixtureEnv(t *testing.T) *env {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	e.feed(time.Time{}, fixtureLines(t)...)
	if _, err := e.project().Recompute([]string{"2026-11-10", "2026-11-11", "2026-11-12"}); err != nil {
		t.Fatal(err)
	}
	return e
}

var fixDays = Spec{Since: "2026-11-10", Until: "2026-11-12T23:59:59Z"}

func spec(kind string, f func(*Spec)) Spec {
	s := fixDays
	s.Kind = kind
	if f != nil {
		f(&s)
	}
	return s
}

func TestQueryTrends(t *testing.T) {
	e := fixtureEnv(t)
	r := e.query(spec("trends", func(s *Spec) { s.Events = []string{"episode_viewed"} }))
	if r.Source != "rollup" || !eq(points(r.Series[0]), []float64{2, 1, 0}) || r.Series[0].Total != 3 || r.ExcludedTest != 1 {
		t.Fatalf("total: %+v", r)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"episode_viewed"}; s.IncludeTest = true }))
	if !eq(points(r.Series[0]), []float64{3, 1, 0}) || r.ExcludedTest != 0 {
		t.Fatalf("with test: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"any"}; s.Math = "uniques" }))
	if !eq(points(r.Series[0]), []float64{5, 2, 1}) {
		t.Fatalf("active: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"any"}; s.Math = "uniques"; s.Interval = "week" }))
	if len(r.Series[0].Points) != 1 || r.Series[0].Points[0].T != "2026-11-09" || r.Series[0].Points[0].V != 5 {
		t.Fatalf("week: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"any"}; s.Math = "wau" }))
	if !eq(points(r.Series[0]), []float64{5, 5, 5}) {
		t.Fatalf("wau: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"any"}; s.Math = "dau"; s.Interval = "month" }))
	if r.Series[0].Points[0].V != round2(8.0/3) {
		t.Fatalf("dau: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"episode_viewed"}; s.Breakdown = "episode_number" }))
	if len(r.Series) != 3 || r.Series[0].Breakdown != "1" || r.Series[0].Total != 1 {
		t.Fatalf("breakdown: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) {
		s.Events = []string{"episode_viewed"}
		s.Where = map[string]string{"episode_number": "3"}
	}))
	if r.Source != "rollup" || !eq(points(r.Series[0]), []float64{0, 1, 0}) {
		t.Fatalf("where: %+v", r)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"episode_viewed"}; s.Math = "per_user_avg" }))
	if r.Series[0].Points[0].V != 2 {
		t.Fatalf("per user: %+v", r.Series)
	}

	// From the raw window: a property filter, a sum, an hourly series, a person filter.
	r = e.query(spec("trends", func(s *Spec) {
		s.Events = []string{"episode_viewed"}
		s.Where = map[string]string{"p.completed": "true"}
	}))
	if r.Source != "raw" || r.Series[0].Total != 2 || r.ExcludedTest != 1 {
		t.Fatalf("raw where: %+v", r)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"purchase_completed"}; s.Math = "sum"; s.Prop = "price_micros" }))
	if r.Series[0].Total != 2990000 {
		t.Fatalf("sum: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) {
		s.Events = []string{"episode_viewed"}
		s.Interval = "hour"
		s.Since, s.Until = "2026-11-10T09:00:00Z", "2026-11-10T11:00:00Z"
	}))
	if len(r.Series[0].Points) != 2 || r.Series[0].Points[0].V != 2 {
		t.Fatalf("hour: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) {
		s.Events = []string{"episode_viewed"}
		s.Where = map[string]string{"person.role": "viewer"}
		s.Math = "uniques"
	}))
	if r.Series[0].Points[0].V != 1 {
		t.Fatalf("person where: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"episode_viewed"}; s.Breakdown = "first_source"; s.Math = "dau" }))
	if r.Series[0].Breakdown != "organic" {
		t.Fatalf("first_source breakdown: %+v", r.Series)
	}
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"$session_end"}; s.Math = "p50"; s.Prop = "duration_ms" }))
	if r.Series[0].Total != 600000 {
		t.Fatalf("p50: %+v", r.Series)
	}
	// Compare: the previous period has nothing.
	r = e.query(spec("trends", func(s *Spec) { s.Events = []string{"episode_viewed"}; s.Compare = true }))
	if len(r.Series[0].Previous) != 3 || r.Series[0].Previous[0].V != 0 {
		t.Fatalf("compare: %+v", r.Series)
	}
	for _, bad := range []Spec{
		spec("trends", nil),
		spec("trends", func(s *Spec) { s.Events = []string{"x_y"}; s.Math = "median" }),
		spec("trends", func(s *Spec) { s.Events = []string{"x_y"}; s.Math = "sum" }),
		spec("trends", func(s *Spec) { s.Events = []string{"x_y"}; s.Interval = "year" }),
		spec("nope", nil),
		{Kind: "trends", Events: []string{"x_y"}, Since: "tomorrowish"},
	} {
		if _, err := e.project().Query(context.Background(), bad); err == nil {
			t.Errorf("%+v: expected an error", bad)
		}
	}
}

func TestQueryFunnelRetentionAndMore(t *testing.T) {
	e := fixtureEnv(t)
	r := e.query(spec("funnel", func(s *Spec) { s.Events = []string{"$first_open", "registration_started", "episode_viewed"} }))
	st := r.Funnels[0].Steps
	if st[0].Count != 5 || st[1].Count != 1 || st[2].Count != 1 || *st[1].FromPrev != 0.2 || *st[2].MedianSeconds != 180 {
		t.Fatalf("funnel: %+v", r.Funnels)
	}
	r = e.query(spec("funnel", func(s *Spec) {
		s.Events = []string{"$first_open", "episode_viewed:episode_number=2"}
		s.Breakdown = "pf"
	}))
	if len(r.Funnels) != 3 || r.Funnels[0].Breakdown != "android" || r.Funnels[0].Steps[1].Count != 1 {
		t.Fatalf("funnel breakdown: %+v", r.Funnels)
	}
	r = e.query(spec("funnel", func(s *Spec) { s.Events = []string{"$first_open", "registration_started"}; s.Order = "strict" }))
	if r.Funnels[0].Steps[1].Count != 0 {
		t.Fatalf("strict: a session start and two screens come between: %+v", r.Funnels)
	}
	r = e.query(spec("funnel", func(s *Spec) {
		s.Events = []string{"$first_open", "episode_viewed", "registration_started"}
		s.Order = "any"
	}))
	if r.Funnels[0].Steps[2].Count != 1 {
		t.Fatalf("any order: %+v", r.Funnels)
	}
	r = e.query(spec("funnel", func(s *Spec) { s.Events = []string{"$first_open", "episode_viewed"}; s.Window = "1m" }))
	if r.Funnels[0].Steps[1].Count != 0 {
		t.Fatalf("window: %+v", r.Funnels)
	}

	r = e.query(spec("retention", func(s *Spec) { s.Period = "day" }))
	if c := r.Cohorts[0]; c.Size != 5 || fmt.Sprint(c.Values[:3]) != "[5 2 1]" {
		t.Fatalf("retention: %+v", r.Cohorts)
	}
	r = e.query(spec("retention", func(s *Spec) { s.Period = "day"; s.Breakdown = "first_source" }))
	found := false
	for _, c := range r.Cohorts {
		if c.Start == "2026-11-10" && c.Breakdown == "organic" {
			found = c.Size == 1 && c.Values[1] == 1
		}
	}
	if !found {
		t.Fatalf("retention by source: %+v", r.Cohorts)
	}
	r = e.query(spec("retention", func(s *Spec) { s.Period = "day"; s.Start = "$first_open"; s.Return = "episode_viewed" }))
	if r.Cohorts[0].Size != 5 || r.Cohorts[0].Values[1] != 1 {
		t.Fatalf("retention by events: %+v", r.Cohorts)
	}

	r = e.query(spec("stickiness", func(s *Spec) { s.Event = "any" }))
	got := map[int]int{}
	for _, row := range r.Rows {
		got[row["days"].(int)] = row["actors"].(int)
	}
	if got[1] != 3 || got[2] != 1 || got[3] != 1 {
		t.Fatalf("stickiness: %+v", r.Rows)
	}

	r = e.query(spec("lifecycle", func(s *Spec) { s.Event = "any"; s.Interval = "day" }))
	if r.Rows[0]["new"] != 5 || r.Rows[1]["returning"] != 2 || r.Rows[1]["dormant"] != -3 || r.Rows[1]["new"] != 0 {
		t.Fatalf("lifecycle: %+v", r.Rows)
	}

	r = e.query(spec("paths", func(s *Spec) { s.From = "main" }))
	if r.Rows[0]["path"] != "main → drama" {
		t.Fatalf("paths: %+v", r.Rows)
	}
	r = e.query(spec("paths", nil))
	if r.Rows[0]["path"] != "main → drama" || r.Rows[0]["count"] != 1 {
		t.Fatalf("transitions: %+v", r.Rows)
	}

	r = e.query(spec("person", func(s *Spec) { s.User = "870" }))
	if r.Person.Props["role"] != "viewer" || r.Person.Actor.FirstSource != "organic" || len(r.Person.Events) < 10 {
		t.Fatalf("person: %+v", r.Person)
	}
	r = e.query(spec("events", func(s *Spec) { s.Event = "episode_viewed" }))
	if len(r.Events) != 3 {
		t.Fatalf("events: %d", len(r.Events))
	}
	r = e.query(spec("events", func(s *Spec) { s.User = "38" }))
	if len(r.Events) != 3 {
		t.Fatalf("events of a test user asked by id: %d", len(r.Events))
	}
	r = e.query(spec("events", func(s *Spec) { s.Device = "5d1c0000-0000-4000-8000-0000000000b1"; s.Limit = 1 }))
	if len(r.Events) != 1 || !strings.Contains(string(r.Events[0]), "$app_updated") {
		t.Fatalf("events by device: %s", r.Events)
	}

	r = e.query(Spec{Kind: "catalog"})
	var ev Row
	for _, row := range r.Rows {
		if row["event"] == "episode_viewed" {
			ev = row
		}
	}
	if ev == nil || ev["n30"] != 4 || fmt.Sprint(ev["dm"]) != "[episode_number]" {
		t.Fatalf("catalog: %+v", ev)
	}

	r = e.query(spec("installs", nil))
	rows := map[string]Row{}
	for _, row := range r.Rows {
		rows[row["source"].(string)] = row
	}
	if o := rows["organic"]; o["install"] != 1 || o["reinstall"] != 1 || o["d1"] != 1.0 {
		t.Fatalf("installs organic: %+v", rows)
	}
	if rows["apple_search_ads"]["install"] != 1 || rows["appstore"]["reinstall"] != 1 || rows["google"]["first_visit"] != 1 {
		t.Fatalf("installs: %+v", rows)
	}
	if !strings.Contains(r.Note, "1 suspicious") {
		t.Fatalf("note %q", r.Note)
	}
	if _, err := e.project().Query(context.Background(), spec("installs", func(s *Spec) { s.Breakdown = "nope" })); err == nil {
		t.Fatal("unknown installs breakdown")
	}
	if _, err := e.project().Query(context.Background(), spec("funnel", func(s *Spec) { s.Events = []string{"a_b"} })); err == nil {
		t.Fatal("a one-step funnel")
	}
	if _, err := e.project().Query(context.Background(), spec("person", nil)); err == nil {
		t.Fatal("person without a user")
	}
}

// Late events and late identify: rollups of the last 8 days are recomputed, so an event that
// arrives days later counts in its own day, and anonymous events go to the person who signs in.
func TestLateEventsAndMerge(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	ev := func(id, ts, name, u, d string) string {
		us := ""
		if u != "" {
			us = `,"u":"` + u + `"`
		}
		return fmt.Sprintf(`{"$":"analytics","v":1,"id":"%s","ts":"%s","e":"%s","d":"%s"%s,"app":"mobile","pf":"ios"}`, id, ts, name, d, us)
	}
	e.feed(time.Time{}, ev("1", "2026-11-14T10:00:00Z", "$pageview", "", "dev1"))
	e.feed(time.Time{}, ev("2", "2026-11-01T10:00:00Z", "$pageview", "", "dev2")) // will sign in too late
	e.feed(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), ev("3", "2026-11-15T10:00:00Z", "$pageview", "", "dev1"))
	e.feed(time.Time{}, ev("4", "2026-11-20T10:00:00Z", "$identify", "7", "dev1"), ev("5", "2026-11-20T10:00:00Z", "$identify", "8", "dev2"))
	p := e.project()
	e.m.Tick(context.Background()) // the hourly pass: the last 8 days
	r14, _ := LoadRollup(p.dir, "2026-11-14")
	r15, _ := LoadRollup(p.dir, "2026-11-15")
	if r14 == nil || r14.Persons.Card() != 1 || r15 == nil || r15.Events["$pageview"].N != 1 {
		t.Fatalf("14th: %+v 15th: %+v", r14, r15)
	}
	if r01, _ := LoadRollup(p.dir, "2026-11-01"); r01 != nil {
		t.Fatal("a day older than 8 days was recomputed")
	}
	// An explicit recompute of the old day still shows it anonymous: the 1st is older than the
	// window in which a link rewrites rollups? No: Recompute uses the current links.
	p.Recompute([]string{"2026-11-01"})
	r01, _ := LoadRollup(p.dir, "2026-11-01")
	if r01.Persons.Card() != 1 {
		t.Fatalf("an explicit recompute uses today's links: %+v", r01.Persons.Card())
	}
	// The quick pass (15 minutes later) touches only today and yesterday.
	e.now = e.now.Add(16 * time.Minute)
	before, _ := os.Stat(rollupPath(p.dir, "2026-11-14"))
	e.m.Tick(context.Background())
	after, _ := os.Stat(rollupPath(p.dir, "2026-11-14"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the quick pass rewrote an old day")
	}
}

func TestArchiveRebuildAndURLs(t *testing.T) {
	f := newFakeS3(t)
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC) // a Sunday, after 01:30
	e := newEnv(t, now)
	e.m.HTTP = f.srv.Client()
	cfg := config.AnalyticsConf{Archive: &config.ArchiveConf{Endpoint: f.srv.URL, Region: "auto", Bucket: "bucket"}}
	e.configure(cfg, nil)
	p := e.project()
	if err := p.ArchiveDue(context.Background()); err == nil || !strings.Contains(err.Error(), "s3-key, s3-secret, s3-prefix") {
		t.Fatalf("archive without secrets: %v", err)
	}
	e.configure(cfg, map[string]string{SecretS3Key: "key", SecretS3Secret: f.secret, SecretS3Prefix: "/analytics/0123456789abcdef/"})
	lines := fixtureLines(t)
	// Received on the 13th and the 14th: the fixture's 10th–12th arrive late, as an offline
	// queue would deliver them.
	e.feed(time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC), lines[:15]...)
	e.feed(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), lines[15:]...)
	if _, err := p.Recompute([]string{"2026-11-10", "2026-11-11", "2026-11-12"}); err != nil {
		t.Fatal(err)
	}
	f.fail = 1
	if err := p.ArchiveDue(context.Background()); err == nil {
		t.Fatal("a failed upload is an error")
	}
	if p.Status().Archive.LastError == "" {
		t.Fatal("the failure is not in the status")
	}
	if err := p.ArchiveDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := "analytics/0123456789abcdef/v1/shop"
	raw13, ok := f.objects[base+"/raw/2026/11/2026-11-13.ndjson.zst"]
	if !ok {
		t.Fatalf("objects: %v", keysOfBytes(f.objects))
	}
	plain, _ := decompress(raw13)
	if strings.Count(string(plain), "\n") != 15 || !strings.HasPrefix(string(plain), lines[0]) {
		t.Fatalf("raw day: %q", plain)
	}
	if f.types[base+"/raw/2026/11/2026-11-13.ndjson.zst"] != "application/zstd" {
		t.Fatal(f.types)
	}
	if _, ok := f.objects[base+"/rollup/2026/2026-11-10.roll.zst"]; !ok {
		t.Fatalf("rollup not uploaded: %v", keysOfBytes(f.objects))
	}
	if _, ok := f.objects[base+"/state/2026-11-15.ndjson.zst"]; !ok {
		t.Fatalf("no Sunday snapshot: %v", keysOfBytes(f.objects))
	}
	puts := f.puts
	if err := p.ArchiveDue(context.Background()); err != nil || f.puts != puts {
		t.Fatalf("a second run uploaded again (%d → %d): %v", puts, f.puts, err)
	}
	// The 15th (today) is not uploaded; days before the stream began count as empty.
	if _, ok := f.objects[base+"/raw/2026/11/2026-11-15.ndjson.zst"]; ok {
		t.Fatal("today was uploaded")
	}
	urls, err := p.ArchiveURLs(context.Background(), time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), time.Hour, true)
	if err != nil || len(urls) != 2 {
		t.Fatalf("urls %v %v", urls, err)
	}
	resp, err := http.Get(urls[1])
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("presigned: %v %v", resp, err)
	}
	resp.Body.Close()

	// A lost disk: a new server with nothing but the config and the secrets rebuilds from the
	// archive and gets the same rollups.
	want, _ := LoadRollup(p.dir, "2026-11-10")
	e2 := newEnv(t, now.AddDate(0, 0, 60)) // the days are long out of the hot window
	e2.m.HTTP = f.srv.Client()
	e2.configure(cfg, map[string]string{SecretS3Key: "key", SecretS3Secret: f.secret, SecretS3Prefix: "analytics/0123456789abcdef"})
	var progress []string
	if err := e2.project().Rebuild(context.Background(), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), func(s string) { progress = append(progress, s) }); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadRollup(e2.project().dir, "2026-11-10")
	if got == nil || got.Active.Card() != want.Active.Card() || got.Events["episode_viewed"].N != want.Events["episode_viewed"].N || got.Installs.Get(KindReinstall).N != want.Installs.Get(KindReinstall).N {
		t.Fatalf("rebuilt %+v want %+v", got, want)
	}
	if !strings.Contains(strings.Join(progress, "\n"), "2026-11-13: 15 records (archive)") {
		t.Fatalf("progress: %v", progress)
	}
	e2.project().state.View(func(tx *Tx) error {
		if tx.Link("5d1c0000-0000-4000-8000-0000000000a1") != "870" {
			t.Error("links not rebuilt")
		}
		return nil
	})
	// Raw queries beyond the hot window point at the archive.
	r := e2.query(Spec{Kind: "events", Since: "2026-11-10", Until: "2026-11-14"})
	if !r.NeedsArchive || !r.Partial || len(r.Archive) == 0 {
		t.Fatalf("needs archive: %+v", r)
	}
	// Without an archive, a rebuild before the window is refused.
	e3 := newEnv(t, now.AddDate(0, 0, 60))
	e3.configure(config.AnalyticsConf{}, nil)
	if err := e3.project().Rebuild(context.Background(), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), nil); err == nil {
		t.Fatal("rebuild before the window without an archive")
	}
}

func keysOfBytes(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRebuildFromStream(t *testing.T) {
	e := fixtureEnv(t)
	p := e.project()
	want, _ := LoadRollup(p.dir, "2026-11-10")
	// Lose the index; the stream is still in the hot window.
	if err := p.Rebuild(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadRollup(p.dir, "2026-11-10")
	if got.Active.Card() != want.Active.Card() || got.Installs.Get(KindInstall).N != 2 || got.Test.Active.Card() != 1 {
		t.Fatalf("rebuilt %+v", got)
	}
	// The config survives a rebuild.
	if p.Config() == nil {
		t.Fatal("config lost")
	}
}

func TestDigest(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 11, 6, 0, 0, 0, time.UTC))
	hour := 7
	e.configure(config.AnalyticsConf{
		Archive: &config.ArchiveConf{Endpoint: "https://s3.example.com", Bucket: "b"},
		Digest:  &config.DigestConf{Hour: &hour, Name: "Drama<Mint>", KeyEvents: []string{"episode_viewed", "user_signed_up"}},
	}, map[string]string{SecretTelegramToken: "tok", SecretTelegramChat: "-100"})
	e.feed(time.Time{}, fixtureLines(t)...)
	k := logs.Key{Project: "shop", Kind: logs.KindMetric, Name: "server"}
	e.ls.Append(k, logs.Line{T: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC).UnixNano(),
		L: `{"$":"metric","ts":"2026-11-10T10:00:00.000Z","name":"analytics_dropped_total","kind":"counter","value":14,"labels":{"reason":"unknown"}}`})
	e.ls.Append(k, logs.Line{T: time.Date(2026, 11, 10, 10, 1, 0, 0, time.UTC).UnixNano(),
		L: `{"$":"metric","ts":"2026-11-10T10:01:00.000Z","name":"analytics_dropped_total","kind":"counter","value":2,"labels":{"reason":"rate"}}`})
	ctx := context.Background()
	e.m.Tick(ctx) // 06:00: rollups, no digest yet
	if len(e.tg.all()) != 0 {
		t.Fatal("digest before its hour")
	}
	e.now = time.Date(2026, 11, 11, 7, 5, 0, 0, time.UTC)
	e.tg.fail = errors.New("telegram is down")
	e.m.Tick(ctx)
	e.tg.fail = nil
	e.now = e.now.Add(time.Minute)
	e.m.Tick(ctx) // the failed send is retried
	e.m.Tick(ctx) // and sent once
	msgs := e.tg.all()
	if len(msgs) != 1 || e.tok != "tok" || e.chat != "-100" {
		t.Fatalf("messages %d, bot %s chat %s", len(msgs), e.tok, e.chat)
	}
	text := msgs[0]
	for _, want := range []string{
		"ℹ️ <b>Аналитика Drama&lt;Mint&gt; за 10.11</b> (UTC)",
		"Активные: <code>DAU 5</code>",
		"<code>WAU 5</code>",
		"Новые: установки <code>2</code> · переуст. <code>1</code> · первый визит веб <code>1</code> · регистрации <code>1</code>",
		"Установки: <code>ios 2 · android 1</code> | <code>ASA 1 · appstore 1 · organic 1</code> | <code>DE 1 · RU 1 · US 1</code>",
		"Деньги: покупки <code>1</code>",
		"Ключевые: episode_viewed <code>2</code> (1 чел.) · user_signed_up <code>1</code> (1 чел.)",
		"Сессии: <code>4</code>, медиана <code>10 мин</code>",
		"⚠️ архив не выгружен за 7 дн. (с 03.11)",
		"выброшено сервером: rate 2, unknown 14",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	// Small numbers: no percentages, and no versions line under 20 actives.
	if strings.Contains(text, "%") || strings.Contains(text, "Версии") {
		t.Errorf("shares printed for a small sample:\n%s", text)
	}
	// English, and a day with nothing.
	p := e.project()
	en := config.AnalyticsConf{Digest: &config.DigestConf{Lang: "en"}}
	p.SetConfig(en)
	text, _ = p.Digest("2026-11-01")
	if !strings.Contains(text, "Analytics shop for 01.11") || !strings.Contains(text, "no events") {
		t.Fatal(text)
	}
	if _, err := p.Digest("yesterday"); err == nil {
		t.Fatal("bad day")
	}
}

func TestDigestShares(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	var lines []string
	for day := 10; day <= 11; day++ {
		for i := 0; i < 40; i++ {
			av := "2.3.1"
			if i%4 == 0 {
				av = "2.3.0"
			}
			if i%10 == 0 {
				av = "2.2.0"
			}
			if day == 11 && i >= 30 {
				continue // 30 of the 40 come back
			}
			lines = append(lines, fmt.Sprintf(`{"$":"analytics","v":1,"id":"%d-%d","ts":"2026-11-%dT10:00:00Z","e":"$pageview","d":"d%d","s":"s%d-%d","app":"mobile","pf":"android","av":"%s","dm":{"screen_name":"main"}}`, day, i, day, i, day, i, av))
		}
	}
	lines = append(lines, `{"$":"analytics","v":1,"id":"err1","ts":"2026-11-11T10:00:00Z","e":"$client_error","d":"d1","app":"mobile","pf":"android","dm":{"fatal":"1","kind":"native"}}`)
	e.feed(time.Time{}, lines...)
	p := e.project()
	p.Recompute([]string{"2026-11-10", "2026-11-11"})
	text, err := p.Digest("2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DAU 30</code> (ср. 7 дн. 40, -25%)", "D1 когорты 10.11: <code>75%</code> (из 40)", "без падений <code>96.7%</code> польз.", "Версии: <code>2.3.1"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}

func TestInstallNotifications(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 10, 0, 0, time.UTC)
	e := newEnv(t, now)
	hour := 7
	cfg := config.AnalyticsConf{Installs: config.InstallsEach, Digest: &config.DigestConf{Hour: &hour, Name: "Shop", Off: true}}
	e.configure(cfg, map[string]string{SecretTelegramToken: "t", SecretTelegramChat: "c"})
	install := func(i int, ik string, extra string) string {
		return fmt.Sprintf(`{"$":"analytics","v":1,"id":"fo%d","ts":"2026-11-16T14:%02d:00Z","e":"$first_open","d":"d%d","app":"mobile","pf":"android","ff":"phone","av":"2.3.1","cc":"DE","dm":{"source":"tiktok","campaign":"spring"%s},"ik":"%s"}`, i, i%60, i, extra, ik)
	}
	e.feed(now, install(1, "k1", ""))
	e.feed(now, install(2, "k1", ""))               // the same key: a reinstall
	e.feed(now, install(3, "k3", `,"suspect":"1"`)) // suspicious: no message
	e.feed(now, `{"$":"analytics","v":1,"id":"web","ts":"2026-11-16T14:01:00Z","e":"$first_open","d":"w","app":"landing","pf":"web"}`)
	waitMsgs := func(n int) []string {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if m := e.tg.all(); len(m) >= n {
				return m
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("expected %d messages, got %v", n, e.tg.all())
		return nil
	}
	msgs := waitMsgs(2)
	joined := strings.Join(msgs, "\n---\n")
	for _, want := range []string{"ℹ️ <b>Установка</b> · Shop", "Платформа: <code>android 2.3.1 · phone</code>", "Страна: <code>DE</code>", "Источник: <code>utm:tiktok · spring</code>", "Повторная: <code>да, 2-я</code>"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	// Over the hourly cap: the rest goes into the summary of the hour.
	for i := 10; i < 40; i++ {
		e.feed(now, install(i, fmt.Sprintf("k%d", i), ""))
	}
	msgs = waitMsgs(20)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.tg.all()); n != 20 {
		t.Fatalf("%d per-install messages, want the cap of 20", n)
	}
	e.now = time.Date(2026, 11, 16, 15, 2, 0, 0, time.UTC)
	e.m.Tick(context.Background())
	msgs = e.tg.all()
	sum := msgs[len(msgs)-1]
	for _, want := range []string{"Установки за 14:00–15:00 UTC</b> · Shop", "Всего: <code>32</code> (переуст. 1, подозрит. 1)", "Платформы: <code>android 32</code>", "Источники: <code>utm:tiktok 32</code>", "Страны: <code>DE 32</code>"} {
		if !strings.Contains(sum, want) {
			t.Errorf("missing %q in\n%s", want, sum)
		}
	}
	e.m.Tick(context.Background())
	if len(e.tg.all()) != len(msgs) {
		t.Fatal("the hourly summary went twice")
	}

	// hourly: no per-install messages, one summary.
	e2 := newEnv(t, now)
	cfg.Installs = config.InstallsHourly
	e2.configure(cfg, map[string]string{SecretTelegramToken: "t", SecretTelegramChat: "c"})
	e2.feed(now, install(1, "k1", ""))
	time.Sleep(50 * time.Millisecond)
	if len(e2.tg.all()) != 0 {
		t.Fatal("hourly mode sent a per-install message")
	}
	e2.now = time.Date(2026, 11, 16, 15, 2, 0, 0, time.UTC)
	e2.m.Tick(context.Background())
	if m := e2.tg.all(); len(m) != 1 || !strings.Contains(m[0], "Всего: <code>1</code>") {
		t.Fatalf("hourly: %v", m)
	}
	// An hour without installs sends nothing.
	e2.now = time.Date(2026, 11, 16, 16, 2, 0, 0, time.UTC)
	e2.m.Tick(context.Background())
	if len(e2.tg.all()) != 1 {
		t.Fatal("a summary of nothing")
	}
}

func TestAccessDigest(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{Digest: &config.DigestConf{Access: true, Name: "Blog"}}, nil)
	k := logs.Key{Project: "shop", Kind: logs.KindAccess, Name: "_"}
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	add := func(ip, uri, ref, ua string, status int) {
		b, _ := json.Marshal(map[string]any{"time": at.Format(time.RFC3339), "ip": ip, "host": "blog.example.com", "method": "GET", "uri": uri, "status": status, "ref": ref, "ua": ua})
		e.ls.Append(k, logs.Line{T: at.UnixNano(), L: string(b)})
	}
	chrome := "Mozilla/5.0 (Macintosh) Chrome/130"
	add("1.1.1.1", "/", "https://www.google.com/search", chrome, 200)
	add("1.1.1.1", "/posts/go?x=1", "https://blog.example.com/", chrome, 200)
	add("2.2.2.2", "/posts/go", "https://news.ycombinator.com/item", "Mozilla/5.0 Safari", 200)
	add("3.3.3.3", "/", "", "Googlebot/2.1", 200)           // bot
	add("1.1.1.1", "/_next/static/app.js", "", chrome, 200) // static
	add("1.1.1.1", "/logo.png", "", chrome, 200)            // static
	add("1.1.1.1", "/missing", "", chrome, 404)             // not a page view
	text, err := e.project().Digest("2026-11-10")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Просмотры: <code>3</code> · посетители ≈<code>2</code>", "Страницы: <code>/posts/go 2 · / 1</code>", "Откуда: <code>google.com 1 · news.ycombinator.com 1</code>"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}

func TestStatusAndConfig(t *testing.T) {
	e := fixtureEnv(t)
	p := e.project()
	e.now = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	s := p.Status()
	if !s.Configured || s.Rollups != 3 || len(s.Services) != 1 || s.Last24h != 4 || s.DiskBytes != 0 || s.Archive.On {
		t.Fatalf("status %+v", s)
	}
	if err := p.SetSecret("nope", "x"); err == nil {
		t.Fatal("unknown secret")
	}
	p.SetSecret(SecretTelegramChat, " -100 ")
	if fmt.Sprint(p.SecretsSet()) != "[telegram-chat]" || p.secrets()[SecretTelegramChat] != "-100" {
		t.Fatal(p.SecretsSet())
	}
	p.SetSecret(SecretTelegramChat, "")
	if len(p.SecretsSet()) != 0 {
		t.Fatal("secret not removed")
	}
	if ch, _ := p.SetConfig(config.AnalyticsConf{}); ch {
		t.Fatal("the same config counted as a change")
	}
	if _, err := p.SetConfig(config.AnalyticsConf{Installs: "sometimes"}); err == nil {
		t.Fatal("invalid config stored")
	}
	if err := p.DeleteConfig(); err != nil || p.Config() != nil {
		t.Fatal("config not deleted")
	}
	// A dry run before deploy.yml has the block renders with the defaults.
	if text, err := p.Digest("2026-11-10"); err != nil || !strings.Contains(text, "Аналитика shop") {
		t.Fatal(text, err)
	}
}

func TestManagerProjectsAndRename(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	// Lines of other kinds and of the platform are not analytics.
	e.m.OnLine(logs.Key{Project: "shop", Kind: logs.KindApp, Name: "server"}, logs.Line{L: Mark})
	e.m.OnLine(logs.Key{Project: logs.Platform, Kind: logs.KindAnalytics, Name: "x"}, logs.Line{L: Mark})
	e.m.OnLine(logs.Key{Project: "shop", Kind: logs.KindAnalytics, Name: "server"}, logs.Line{L: `{"$":"analytics","v":9}`})
	if len(e.m.Projects()) != 0 {
		t.Fatalf("projects %v", e.m.Projects())
	}
	e.feed(time.Time{}, fixtureLines(t)[0])
	if fmt.Sprint(e.m.Projects()) != "[shop]" {
		t.Fatal(e.m.Projects())
	}
	e.m.RenameProject("shop", "store")
	if _, err := os.Stat(filepath.Join(e.m.Dir, "store", "state.db")); err != nil {
		t.Fatal("not moved")
	}
	p, _ := e.m.Project("store")
	var cat Catalog
	p.state.GetMeta("catalog", &cat)
	if cat.Events[EventFirstOpen] == nil {
		t.Fatal("the renamed project lost its state")
	}
	if _, err := e.m.Project("../x"); err == nil {
		t.Fatal("a path as a project name")
	}
	e.m.RenameProject("ghost", "other") // nothing to move: no error, no directory
	if _, err := os.Stat(filepath.Join(e.m.Dir, "other")); err == nil {
		t.Fatal("rename created a directory")
	}
}
