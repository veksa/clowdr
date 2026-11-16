package analytics

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
)

// synth is a generated project: a few thousand events of devices that install, browse, sign in
// (some as test accounts), watch and buy over two weeks, with duplicates and late arrivals. The
// queries are checked against a naive count over the same records.
type synth struct {
	lines []string
	recv  []time.Time
	recs  []Record
}

func genSynth(t *testing.T, start time.Time, days int) *synth {
	rng := rand.New(rand.NewSource(42))
	s := &synth{}
	id := 0
	add := func(ts time.Time, ev, u, d string, extra string) {
		id++
		us := ""
		if u != "" {
			us = fmt.Sprintf(`,"u":"%s"`, u)
		}
		ds := ""
		if d != "" {
			ds = fmt.Sprintf(`,"d":"%s"`, d)
		}
		line := fmt.Sprintf(`{"$":"analytics","v":1,"id":"e%05d","ts":"%s","e":"%s"%s%s,"app":"mobile","pf":"%s","av":"2.%d.0","cc":"%s"%s}`,
			id, ts.Format(time.RFC3339Nano), ev, us, ds, []string{"ios", "android"}[id%2], id%3, []string{"DE", "US", "RU", "FR"}[id%4], extra)
		recv := ts.Add(time.Duration(rng.Intn(30)) * time.Second)
		if rng.Intn(25) == 0 {
			recv = ts.Add(time.Duration(1+rng.Intn(6)) * 24 * time.Hour) // an offline queue
		}
		s.lines = append(s.lines, line)
		s.recv = append(s.recv, recv)
		if rng.Intn(60) == 0 { // a retried batch: the same record again
			s.lines = append(s.lines, line)
			s.recv = append(s.recv, recv.Add(time.Minute))
		}
	}
	for dev := 0; dev < 240; dev++ {
		d := fmt.Sprintf("dev%03d", dev)
		first := rng.Intn(days)
		user := ""
		if dev%3 != 0 {
			user = fmt.Sprintf("%d", 1000+dev)
		}
		test := dev%17 == 0 && user != ""
		day0 := start.AddDate(0, 0, first).Add(time.Duration(rng.Intn(20)) * time.Hour)
		ik := ""
		if dev%5 != 0 {
			ik = fmt.Sprintf(`,"ik":"k%03d"`, dev%210) // some keys repeat: reinstalls
		}
		add(day0, "$first_open", "", d, fmt.Sprintf(`,"dm":{"source":"%s"}%s`, []string{"organic", "tiktok", "apple_search_ads"}[dev%3], ik))
		signed := false
		for day := first; day < days; day++ {
			if day > first && rng.Intn(3) != 0 {
				continue
			}
			base := start.AddDate(0, 0, day).Add(time.Duration(rng.Intn(20)) * time.Hour)
			n := 1 + rng.Intn(4)
			for k := 0; k < n; k++ {
				ts := base.Add(time.Duration(k) * time.Minute)
				u := ""
				if signed {
					u = user
				}
				add(ts, "$pageview", u, d, fmt.Sprintf(`,"dm":{"screen_name":"%s"}`, []string{"main", "drama", "episode"}[k%3]))
			}
			if user != "" && !signed && rng.Intn(2) == 0 {
				signed = true
				set := ""
				if test {
					set = `,"set":{"test":true}`
				}
				add(base.Add(10*time.Minute), "$identify", user, d, `,"p":{"anon":"`+d+`"}`+set)
			}
			if signed && rng.Intn(2) == 0 {
				add(base.Add(20*time.Minute), "episode_viewed", user, "", fmt.Sprintf(`,"src":"server","dm":{"episode_number":"%d"},"p":{"watch_ms":%d}`, 1+rng.Intn(5), 1000*rng.Intn(600)))
			}
			if signed && rng.Intn(9) == 0 {
				add(base.Add(30*time.Minute), "purchase_completed", user, "", `,"src":"server","dm":{"kind":"coins","store":"google"}`)
			}
		}
	}
	// Receive order is the order clowdr sees.
	idx := make([]int, len(s.lines))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return s.recv[idx[a]].Before(s.recv[idx[b]]) })
	lines, recv := make([]string, len(idx)), make([]time.Time, len(idx))
	for i, j := range idx {
		lines[i], recv[i] = s.lines[j], s.recv[j]
	}
	s.lines, s.recv = lines, recv
	seen := map[string]bool{}
	for _, l := range s.lines {
		r := mustParse(t, l)
		if !seen[r.ID] {
			seen[r.ID] = true
			s.recs = append(s.recs, r)
		}
	}
	return s
}

// naive counts the same things the slow and obvious way.
type naive struct {
	actor    func(*Record) string
	excluded map[string]bool
}

func newNaive(recs []Record) *naive {
	links, test := map[string]string{}, map[string]bool{}
	for i := range recs {
		r := &recs[i]
		if u := r.User(); u != "" {
			dev := r.D
			if r.E == EventIdentify {
				dev = r.anon()
			}
			if dev != "" && links[dev] == "" {
				links[dev] = u
			}
			if r.Set["test"] == true {
				test["u:"+u] = true
			}
		}
	}
	return &naive{excluded: test, actor: func(r *Record) string {
		if u := r.User(); u != "" {
			return "u:" + u
		}
		if u := links[r.D]; u != "" {
			return "u:" + u
		}
		return "d:" + r.D
	}}
}

func TestGoldenAgainstNaive(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	const nDays = 14
	syn := genSynth(t, start, nDays)
	if len(syn.lines) < 2000 || len(syn.lines) > 4000 {
		t.Fatalf("%d lines: the fixture should hold 2–3 thousand events", len(syn.lines))
	}
	e := newEnv(t, start.AddDate(0, 0, nDays+8))
	e.configure(config.AnalyticsConf{}, nil)
	e.feedAt(syn.lines, syn.recv)
	p := e.project()
	all := days(start, start.AddDate(0, 0, nDays-1))
	if _, err := p.Recompute(all); err != nil {
		t.Fatal(err)
	}
	nv := newNaive(syn.recs)

	// Daily active actors, events and their uniques.
	active := map[string]map[string]bool{}
	evN := map[string]int{}
	evU := map[string]map[string]bool{}
	firstDay := map[string]string{}
	for i := range syn.recs {
		r := &syn.recs[i]
		a := nv.actor(r)
		if fd, ok := firstDay[a]; !ok || r.Day() < fd {
			firstDay[a] = r.Day()
		}
		if nv.excluded[a] {
			continue
		}
		d := r.Day()
		if active[d] == nil {
			active[d] = map[string]bool{}
		}
		active[d][a] = true
		if r.E == "episode_viewed" {
			evN[d]++
			if evU[d] == nil {
				evU[d] = map[string]bool{}
			}
			evU[d][a] = true
		}
	}
	window := Spec{Since: all[0], Until: all[len(all)-1] + "T23:59:59Z"}
	q := func(f func(*Spec)) *Result {
		s := window
		f(&s)
		return e.query(s)
	}
	r := q(func(s *Spec) { s.Kind = "trends"; s.Events = []string{"any"}; s.Math = "uniques" })
	for i, d := range all {
		if got := r.Series[0].Points[i].V; int(got) != len(active[d]) {
			t.Errorf("%s: active %v, naive %d", d, got, len(active[d]))
		}
	}
	for _, math := range []string{"total", "uniques"} {
		roll := q(func(s *Spec) { s.Kind = "trends"; s.Events = []string{"episode_viewed"}; s.Math = math })
		raw := q(func(s *Spec) {
			s.Kind = "trends"
			s.Events = []string{"episode_viewed"}
			s.Math = math
			s.Where = map[string]string{"p.none!": "x"} // a property filter: computed from raw records
		})
		if roll.Source != "rollup" || raw.Source != "raw" {
			t.Fatalf("sources %s %s", roll.Source, raw.Source)
		}
		for i, d := range all {
			want := evN[d]
			if math == "uniques" {
				want = len(evU[d])
			}
			if int(roll.Series[0].Points[i].V) != want || int(raw.Series[0].Points[i].V) != want {
				t.Errorf("%s %s: rollup %v raw %v naive %d", math, d, roll.Series[0].Points[i].V, raw.Series[0].Points[i].V, want)
			}
		}
		if roll.ExcludedTest == 0 || roll.ExcludedTest != raw.ExcludedTest {
			t.Errorf("excluded: rollup %d raw %d", roll.ExcludedTest, raw.ExcludedTest)
		}
	}

	// Retention of first-seen cohorts by day.
	ret := q(func(s *Spec) { s.Kind = "retention"; s.Period = "day" })
	for _, c := range ret.Cohorts {
		cohort := map[string]bool{}
		for a, fd := range firstDay {
			if fd == c.Start && !nv.excluded[a] {
				cohort[a] = true
			}
		}
		if c.Size != len(cohort) {
			t.Errorf("cohort %s: %d, naive %d", c.Start, c.Size, len(cohort))
			continue
		}
		for k, v := range c.Values {
			d := dayTime(c.Start).AddDate(0, 0, k).Format(DayLayout)
			want := 0
			for a := range cohort {
				if active[d][a] || k == 0 {
					want++
				}
			}
			if v != want {
				t.Errorf("cohort %s day +%d: %d, naive %d", c.Start, k, v, want)
			}
		}
	}

	// Stickiness: days active per actor.
	st := q(func(s *Spec) { s.Kind = "stickiness"; s.Event = "any" })
	perActor := map[string]int{}
	for _, d := range all {
		for a := range active[d] {
			perActor[a]++
		}
	}
	hist := map[int]int{}
	for _, n := range perActor {
		hist[n]++
	}
	for _, row := range st.Rows {
		if row["actors"].(int) != hist[row["days"].(int)] {
			t.Errorf("stickiness %v: naive %d", row, hist[row["days"].(int)])
		}
	}

	// Funnel: installed, then watched within the window.
	fun := q(func(s *Spec) {
		s.Kind = "funnel"
		s.Events = []string{"$first_open", "episode_viewed"}
		s.Window = "7d"
	})
	byActor := map[string][]*Record{}
	for i := range syn.recs {
		r := &syn.recs[i]
		if a := nv.actor(r); !nv.excluded[a] {
			byActor[a] = append(byActor[a], r)
		}
	}
	step0, step1 := 0, 0
	for _, evs := range byActor {
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].t.Before(evs[j].t) })
		best := 0
		for i, r := range evs {
			if r.E != EventFirstOpen {
				continue
			}
			reached := 1
			for _, r2 := range evs[i+1:] {
				if r2.t.After(r.t.Add(7 * 24 * time.Hour)) {
					break
				}
				if r2.E == "episode_viewed" {
					reached = 2
					break
				}
			}
			best = max(best, reached)
		}
		if best >= 1 {
			step0++
		}
		if best >= 2 {
			step1++
		}
	}
	if s := fun.Funnels[0].Steps; s[0].Count != step0 || s[1].Count != step1 {
		t.Errorf("funnel %d → %d, naive %d → %d", s[0].Count, s[1].Count, step0, step1)
	}

	// Installs and reinstalls: the first $first_open of a key is the install.
	keys := map[string]int{}
	inst, reinst, web := 0, 0, 0
	for i := range syn.recs {
		r := &syn.recs[i]
		if r.E != EventFirstOpen || nv.excluded[nv.actor(r)] {
			if r.E == EventFirstOpen && r.IK != "" {
				keys[r.IK]++
			}
			continue
		}
		switch {
		case r.IK == "":
			web++
		case keys[r.IK] == 0:
			inst++
			keys[r.IK]++
		default:
			reinst++
			keys[r.IK]++
		}
	}
	ins := q(func(s *Spec) { s.Kind = "installs" })
	gi, gr, gw := 0, 0, 0
	for _, row := range ins.Rows {
		gi += row["install"].(int)
		gr += row["reinstall"].(int)
		gw += row["first_visit"].(int)
	}
	if gi != inst || gr != reinst || gw != web {
		t.Errorf("installs %d/%d/%d, naive %d/%d/%d", gi, gr, gw, inst, reinst, web)
	}
	if reinst == 0 || roll(t, p, all).dup == 0 {
		t.Error("the fixture should have reinstalls and duplicates")
	}
}

type rollStats struct{ dup int }

func roll(t *testing.T, p *Project, ds []string) rollStats {
	var s rollStats
	for _, d := range ds {
		if r, _ := LoadRollup(p.dir, d); r != nil {
			s.dup += r.Dropped["dup"]
		}
	}
	return s
}
