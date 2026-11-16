package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
)

// A full index queue (a rebuild holds the index) must not hold the collector: it serves every
// stream of the container. The records it had no room for are indexed from the stream later.
func TestOnLineNeverBlocksAndCatchesUp(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	p := e.project()
	k := logs.Key{Project: "shop", Kind: logs.KindAnalytics, Name: "server"}
	p.indexMu.Lock()
	const n = 6000
	start := time.Now()
	for i := 0; i < n; i++ {
		l := fmt.Sprintf(`{"$":"analytics","v":1,"id":"id-%d","ts":"2026-11-16T10:00:00Z","e":"$pageview","u":"%d","app":"mobile","pf":"ios"}`, i, i)
		line := logs.Line{T: e.now.Add(time.Duration(i) * time.Millisecond).UnixNano(), L: l}
		if err := e.ls.Append(k, line); err != nil {
			t.Fatal(err)
		}
		e.m.OnLine(k, line)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the collector waited %v for the indexer", took)
	}
	p.mu.Lock()
	dropped := p.idx.dropped
	p.mu.Unlock()
	if dropped == 0 {
		t.Fatal("the queue never filled: the test does not test anything")
	}
	p.indexMu.Unlock()
	p.Sync()
	persons := 0
	p.state.View(func(tx *Tx) error {
		tx.EachPerson(func(string, Person) { persons++ })
		return nil
	})
	if persons != n {
		t.Fatalf("indexed %d persons of %d", persons, n)
	}
}

// A rebuild from a later day must not renumber actors: rollups older than `since` keep their
// bitmaps forever, and persons, links and install keys seen before `since` must stay.
func TestRebuildFromALaterDayKeepsTheIndex(t *testing.T) {
	e := fixtureEnv(t)
	p := e.project()
	ids := map[string]uint32{}
	p.state.View(func(tx *Tx) error {
		tx.tx.Bucket(bActors).ForEach(func(k, _ []byte) error {
			a, _ := tx.Actor(string(k))
			ids[string(k)] = a.ID
			return nil
		})
		return nil
	})
	before, _ := LoadRollup(p.dir, "2026-11-10")
	if err := p.Rebuild(context.Background(), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	p.state.View(func(tx *Tx) error {
		for k, id := range ids {
			if a, ok := tx.Actor(k); !ok || a.ID != id {
				t.Errorf("actor %s: %d → %+v", k, id, a)
			}
		}
		if pr, ok := tx.Person("870"); !ok || pr.Props["role"] != "viewer" {
			t.Errorf("person 870 lost: %+v", pr)
		}
		if tx.Link("5d1c0000-0000-4000-8000-0000000000a1") != "870" {
			t.Error("a link from before since lost")
		}
		return nil
	})
	after, _ := LoadRollup(p.dir, "2026-11-10")
	b1, _ := json.Marshal(before.Part)
	b2, _ := json.Marshal(after.Part)
	if string(b1) != string(b2) {
		t.Fatal("a rollup older than since changed")
	}
	// From the first day the index is built again from scratch, with the same result.
	if err := p.Rebuild(context.Background(), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	r, _ := LoadRollup(p.dir, "2026-11-10")
	if r.Active.Card() != before.Active.Card() {
		t.Fatal("full rebuild differs")
	}
}

// Records of an erased account that arrive later (an offline queue) count, but do not bring
// the person or its device links back.
func TestAccountDeletedStaysErased(t *testing.T) {
	st := openState(t)
	rec := func(id, e, extra string) Record {
		return mustParse(t, fmt.Sprintf(`{"$":"analytics","v":1,"id":"%s","ts":"2026-11-10T10:00:00Z","e":"%s","u":"5","d":"dev5","app":"mobile","pf":"ios"%s}`, id, e, extra))
	}
	if _, err := Index(st, []Record{rec("1", "$set", `,"set":{"role":"author","test":true}`), rec("2", "account_deleted", "")}); err != nil {
		t.Fatal(err)
	}
	if _, err := Index(st, []Record{rec("3", "episode_viewed", `,"set":{"role":"viewer"}`)}); err != nil {
		t.Fatal(err)
	}
	st.View(func(tx *Tx) error {
		p, _ := tx.Person("5")
		if !p.Deleted || len(p.Props) != 1 || p.Props["test"] != true || len(p.Devices) != 0 || p.LastTS != "" {
			t.Errorf("erased person came back: %+v", p)
		}
		if tx.Link("dev5") != "" {
			t.Error("the device was linked again")
		}
		return nil
	})
}

func feedSynthetic(e *env) {
	var lines []string
	add := func(id, ts, ev, u, pf string, n int, store string) {
		lines = append(lines, fmt.Sprintf(`{"$":"analytics","v":1,"id":"%s","ts":"%s","e":"%s","u":"%s","s":"s%s","app":"mobile","pf":"%s","dm":{"store":"%s","screen_name":"%s"},"p":{"n":%d}}`,
			id, ts, ev, u, u, pf, store, []string{"main", "drama", "episode"}[n%3], n))
	}
	for i := 1; i <= 10; i++ {
		pf, store := "ios", "a"
		if i%2 == 0 {
			pf = "android"
		}
		if i%3 == 0 {
			store = "b"
		} else if i%5 == 0 {
			store = "c"
		}
		add(fmt.Sprint("v", i), fmt.Sprintf("2026-11-15T10:%02d:00Z", i), "purchase_completed", fmt.Sprint(i), pf, i, store)
		add(fmt.Sprint("p", i), fmt.Sprintf("2026-11-15T09:%02d:00Z", i), "$pageview", fmt.Sprint(i), pf, i, store)
		add(fmt.Sprint("q", i), fmt.Sprintf("2026-11-15T09:%02d:30Z", i), "$pageview", fmt.Sprint(i), pf, i+1, store)
	}
	e.feed(time.Time{}, lines...)
}

func TestFiltersPercentilesAndTwoKeys(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	feedSynthetic(e)
	day := func(s *Spec) {
		s.Since, s.Until, s.Events = "2026-11-15", "2026-11-15T23:59:59Z", []string{"purchase_completed"}
	}
	total := func(f func(*Spec)) float64 {
		t.Helper()
		s := Spec{Kind: "trends"}
		day(&s)
		f(&s)
		r := e.query(s)
		return r.Series[0].Total
	}
	for _, c := range []struct {
		filters []string
		want    float64
	}{
		{[]string{"p.n>=8"}, 3},
		{[]string{"p.n<3"}, 2},
		{[]string{"p.n>2", "p.n<=4"}, 2},
		{[]string{"store=b", "store=c"}, 5}, // b: 3, 6, 9; c: 5, 10
		{[]string{"store=b|c"}, 5},
		{[]string{"store=b,c"}, 5},
		{[]string{"store!=b|c"}, 5},
		{[]string{"store!=a", "pf=ios"}, 3}, // 3, 5, 9
	} {
		if got := total(func(s *Spec) { s.Filters = c.filters }); got != c.want {
			t.Errorf("%v: %v, want %v", c.filters, got, c.want)
		}
	}
	if _, err := e.project().Query(context.Background(), Spec{Kind: "trends", Events: []string{"x_y"}, Filters: []string{"p.n>abc"}}); err == nil {
		t.Error("a comparison with a non-number accepted")
	}
	for m, want := range map[string]float64{"p50": 5, "p75": 8, "p90": 9, "p95": 10, "p99": 10} {
		if got := total(func(s *Spec) { s.Math, s.Prop = m, "n" }); got != want {
			t.Errorf("%s: %v, want %v", m, got, want)
		}
	}
	// Two breakdown keys.
	s := Spec{Kind: "trends", Breakdown: "pf,store"}
	day(&s)
	r := e.query(s)
	got := map[string]float64{}
	for _, ser := range r.Series {
		got[ser.Breakdown] = ser.Total
	}
	if got["ios / a"] != 2 || got["android / a"] != 3 || got["android / b"] != 1 || got["android / c"] != 1 {
		t.Fatalf("two keys: %v", got)
	}
	if _, err := e.project().Query(context.Background(), Spec{Kind: "trends", Events: []string{"x_y"}, Breakdown: "a,b,c"}); err == nil {
		t.Error("three breakdown keys accepted")
	}
	// A funnel applies the global filters to every step.
	f := e.query(Spec{Kind: "funnel", Events: []string{"$pageview", "purchase_completed"}, Since: "2026-11-15", Until: "2026-11-15T23:59:59Z", Filters: []string{"pf=ios"}})
	if f.Funnels[0].Steps[0].Count != 5 || f.Funnels[0].Steps[1].Count != 5 {
		t.Fatalf("funnel with filters: %+v", f.Funnels)
	}
	// Rollup kinds refuse filters instead of ignoring them.
	if _, err := e.project().Query(context.Background(), Spec{Kind: "retention", Filters: []string{"pf=ios"}}); err == nil {
		t.Error("retention ignored a filter")
	}
}

func TestPathsLimitDepthAndScreens(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	feedSynthetic(e)
	base := Spec{Kind: "paths", Since: "2026-11-15", Until: "2026-11-15T23:59:59Z"}
	r := e.query(base)
	if len(r.Rows) == 0 || len(r.Screens) != 3 || r.Columns[0] != "path" || r.ScreenColumns[0] != "screen" {
		t.Fatalf("paths: %+v", r)
	}
	for _, row := range r.Rows {
		if row["path"] == nil {
			t.Fatalf("a row without a path: %+v", row)
		}
	}
	s := base
	s.Limit = 1
	if r := e.query(s); len(r.Rows) != 1 || len(r.Screens) != 1 {
		t.Fatalf("limit 1: %+v", r)
	}
	for _, d := range []int{1, 2, 6} {
		s := base
		s.Depth = d
		if _, err := e.project().Query(context.Background(), s); err == nil {
			t.Errorf("depth %d accepted", d)
		}
	}
}

func TestRawQueryBounds(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	feedSynthetic(e)
	old := maxRaw
	maxRaw = 5
	defer func() { maxRaw = old }()
	_, err := e.project().Query(context.Background(), Spec{Kind: "events", Since: "2026-11-15"})
	if !errors.Is(err, ErrTooManyRecords) {
		t.Fatalf("a query over the bound: %v", err)
	}
	// A person's records are picked before they are kept: 3 of 30 fit the bound.
	r := e.query(Spec{Kind: "person", User: "4", Since: "2026-11-15"})
	if len(r.Person.Events) != 3 {
		t.Fatalf("person: %d events", len(r.Person.Events))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	maxRaw = old
	if _, err := e.project().Query(ctx, Spec{Kind: "events", Since: "2026-11-15"}); err == nil {
		t.Fatal("a cancelled query ran")
	}
}

// A window trimmed by age has lost the start of its first day: no rollup is made up for it or
// the days before from what is left.
func TestNoRollupFromATrimmedWindow(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	e.m.Retention = func() time.Duration { return 2 * 24 * time.Hour }
	e.configure(config.AnalyticsConf{}, nil)
	k := logs.Key{Project: "shop", Kind: logs.KindAnalytics, Name: "server"}
	// Received three days ago (older than the window), about the day before.
	l := `{"$":"analytics","v":1,"id":"x1","ts":"2026-11-12T10:00:00Z","e":"$pageview","u":"1","app":"mobile","pf":"ios"}`
	line := logs.Line{T: now.AddDate(0, 0, -3).UnixNano(), L: l}
	e.ls.Append(k, line)
	e.ls.FlushAll()
	// The store names a segment by the hour it was flushed: move it to the hour it was received.
	dir := filepath.Join(e.ls.DataDir, "shop", logs.KindAnalytics, "server")
	es, _ := os.ReadDir(dir)
	if len(es) != 1 {
		t.Fatalf("segments: %v", es)
	}
	if err := os.Rename(filepath.Join(dir, es[0].Name()), filepath.Join(dir, now.AddDate(0, 0, -3).Format("2006010215")+".zst")); err != nil {
		t.Fatal(err)
	}
	if ws := e.project().WindowStart(); !ws.Equal(now.AddDate(0, 0, -2)) {
		t.Fatalf("window start %v", ws)
	}
	e.query(Spec{Kind: "trends", Events: []string{"$pageview"}, Since: "2026-11-11"})
	if r, _ := LoadRollup(e.project().dir, "2026-11-12"); r != nil {
		t.Fatal("a rollup of a day before the window")
	}
}
