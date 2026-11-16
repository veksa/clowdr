package analytics

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureLines(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("testdata/records.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func mustParse(t *testing.T, line string) Record {
	t.Helper()
	r, err := Parse(line)
	if err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return r
}

func openState(t *testing.T) *State {
	t.Helper()
	st, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestParseAndValidate(t *testing.T) {
	lines := fixtureLines(t)
	ok, bad := 0, 0
	for _, l := range lines {
		r, err := Parse(l)
		if err != nil {
			bad++
			continue
		}
		ok++
		if ps := Validate(&r); len(ps) > 0 {
			t.Errorf("fixture record %s has problems: %v", r.ID, ps)
		}
	}
	if ok != 28 || bad != 2 {
		t.Fatalf("parsed %d, refused %d", ok, bad)
	}
	for line, want := range map[string]string{
		`{"$":"analytics","v":2,"id":"1","ts":"2026-11-10T00:00:00Z","e":"x_y"}`: "unsupported version",
		`{"$":"analytics","v":1,"ts":"2026-11-10T00:00:00Z","e":"x_y"}`:          "no id",
		`{"$":"analytics","v":1,"id":"1","ts":"yesterday","e":"x_y"}`:            "bad ts",
		`{"$":"analytics","v":1,"id":"1","ts":"2026-11-10T00:00:00Z","e":"Bad"}`: "bad event name",
		`{"$":"trace","v":1}`: "not an analytics record",
		`{"$":"analytics","v":1,"id":"1","ts":"2026-11-10T00:00:00Z","e":"x_y","p":{"a":` + "\n": "bad JSON",
	} {
		if _, err := Parse(line); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", line, err, want)
		}
	}
	// A numeric user id is taken as a string.
	r := mustParse(t, `{"$":"analytics","v":1,"id":"1","ts":"2026-11-10T00:00:00Z","e":"x_y","u":870}`)
	if r.User() != "870" {
		t.Fatal(r.User())
	}
	r = mustParse(t, `{"$":"analytics","v":1,"id":"1","ts":"2026-11-10T00:00:00Z","e":"x_y","app":"fridge","pf":"tizen","ff":"car","cc":"de","lc":"rus","src":"x","dm":{"a":1,"b":"`+strings.Repeat("x", 65)+`"},"p":{"o":{"x":1},"s":"`+strings.Repeat("y", 201)+`"},"ik":"k"}`)
	ps := strings.Join(Validate(&r), "\n")
	for _, want := range []string{"no actor", "unknown app", "unknown pf", "unknown ff", "src", "cc", "lc", "dm.a is not a string", "dm.b is longer", "p.o is not", "p.s is longer", "ik outside"} {
		if !strings.Contains(ps, want) {
			t.Errorf("missing %q in\n%s", want, ps)
		}
	}
	for _, n := range []string{"$pageview", "episode_viewed", "a1"} {
		if !ValidEventName(n) {
			t.Error(n)
		}
	}
	for _, n := range []string{"", "a", "Episode", "1abc", "a-b", "$" + strings.Repeat("a", 48)} {
		if ValidEventName(n) {
			t.Error(n)
		}
	}
}

func TestCheckFixture(t *testing.T) {
	rep, err := CheckFiles([]string{"testdata/records.ndjson"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"lines": 30, "records": 30, "invalid": 2, "warnings": 0, "dup": 1, "actors": 6, "persons": 2, "merged": 6, "sessions": 6, "excluded": 1}
	got := map[string]int{"lines": rep.Lines, "records": rep.Records, "invalid": rep.Invalid, "warnings": rep.Warnings, "dup": rep.Dup, "actors": rep.Actors, "persons": rep.Persons, "merged": rep.Merged, "sessions": rep.Sessions, "excluded": rep.Excluded}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	if rep.Installs[KindInstall] != 2 || rep.Installs[KindReinstall] != 2 || rep.Installs[KindFirstVisit] != 1 || rep.Installs["suspect"] != 1 {
		t.Errorf("installs %v", rep.Installs)
	}
	if e := rep.Events["episode_viewed"]; e.N != 4 || e.Actors != 2 {
		t.Errorf("episode_viewed %+v", e)
	}
	if len(rep.Problems) != 2 || len(rep.Days) != 3 {
		t.Errorf("problems %v days %v", rep.Problems, rep.Days)
	}
}

// Reinstall through two files: the second install of the same key in a later file.
func TestCheckReinstallAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	a := `{"$":"analytics","v":1,"id":"i1","ts":"2026-11-10T09:00:00Z","e":"$first_open","d":"d1","app":"mobile","pf":"android","ik":"k1"}`
	b := `{"$":"analytics","v":1,"id":"i2","ts":"2026-11-10T10:00:00Z","e":"$first_open","d":"d2","app":"mobile","pf":"android","ik":"k1"}`
	os.WriteFile(filepath.Join(dir, "a.ndjson"), []byte("[Nest] some log line\n"+a+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.ndjson"), []byte("2026-11-10T10:00:00Z server "+b+"\n"), 0o644)
	rep, err := CheckFiles([]string{filepath.Join(dir, "a.ndjson"), filepath.Join(dir, "b.ndjson")})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Installs[KindInstall] != 1 || rep.Installs[KindReinstall] != 1 || rep.Other != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, err := CheckFiles([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("a missing file is an error")
	}
}

func TestIndexLinksPersonsInstalls(t *testing.T) {
	st := openState(t)
	var recs []Record
	for _, l := range fixtureLines(t) {
		if r, err := Parse(l); err == nil {
			recs = append(recs, r)
		}
	}
	ds, err := Index(st, recs)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, d := range ds {
		kinds[d.Record.D] = fmt.Sprintf("%s/%d", d.Kind, d.N)
	}
	want := map[string]string{
		"5d1c0000-0000-4000-8000-0000000000a1": "install/1",
		"5d1c0000-0000-4000-8000-0000000000b1": "install/1",
		"5d1c0000-0000-4000-8000-0000000000c1": "reinstall/2", // the client's hint
		"5d1c0000-0000-4000-8000-0000000000d1": "first_visit/1",
		"5d1c0000-0000-4000-8000-0000000000a2": "reinstall/2", // the same key as a1
	}
	for d, k := range want {
		if kinds[d] != k {
			t.Errorf("%s: %s, want %s", d, kinds[d], k)
		}
	}
	st.View(func(tx *Tx) error {
		if u := tx.Link("5d1c0000-0000-4000-8000-0000000000a1"); u != "870" {
			t.Errorf("a1 → %q", u)
		}
		p, ok := tx.Person("870")
		if !ok || p.Props["role"] != "viewer" || p.Props["first_app"] != "mobile" || len(p.Devices) != 2 {
			t.Errorf("person 870: %+v", p)
		}
		// The deleted test account keeps only what the exclusion rule reads.
		p38, ok := tx.Person("38")
		if !ok || len(p38.Props) != 1 || p38.Props["test"] != true || p38.FirstTS != "" {
			t.Errorf("person 38 after account_deleted: %+v", p38)
		}
		if tx.Link("5d1c0000-0000-4000-8000-0000000000f1") != "" {
			t.Error("the deleted person's device link stays")
		}
		// The person inherits the install source of the device it signed up on.
		a, _ := tx.Actor("u:870")
		if a.FirstSource != "organic" || a.FirstDay != "2026-11-10" || a.FirstPF != "android" {
			t.Errorf("actor 870: %+v", a)
		}
		k, _ := tx.InstallKey("9f0000000000000000000000000000a1")
		if k.N != 2 || k.FirstDay != "2026-11-10" || k.LastDay != "2026-11-11" {
			t.Errorf("install key: %+v", k)
		}
		return nil
	})
	// Applying the same records again changes no install count (ids are remembered).
	if _, err := Index(st, recs); err != nil {
		t.Fatal(err)
	}
	st.View(func(tx *Tx) error {
		if k, _ := tx.InstallKey("9f0000000000000000000000000000a1"); k.N != 2 {
			t.Errorf("reindexing counted again: %+v", k)
		}
		return nil
	})
	var cat Catalog
	st.GetMeta("catalog", &cat)
	if e := cat.Events["episode_viewed"]; e == nil || e.DM["episode_number"] == "" || e.P["drama_id"] == "" || e.Src["server"] == "" {
		t.Errorf("catalog: %+v", cat.Events["episode_viewed"])
	}
	if cat.Events["user_signed_up"].Set["first_pf"] == "" {
		t.Error("once keys are in the catalog")
	}
}

func TestFirstLinkWins(t *testing.T) {
	st := openState(t)
	r1 := mustParse(t, `{"$":"analytics","v":1,"id":"1","ts":"2026-11-10T09:00:00Z","e":"$identify","u":"1","d":"dev","p":{"anon":"dev"}}`)
	r2 := mustParse(t, `{"$":"analytics","v":1,"id":"2","ts":"2026-11-10T10:00:00Z","e":"$identify","u":"2","d":"dev","p":{"anon":"dev"}}`)
	Index(st, []Record{r1, r2})
	st.View(func(tx *Tx) error {
		if u := tx.Link("dev"); u != "1" {
			t.Errorf("link %q", u)
		}
		return nil
	})
}

func TestComputeDayFixture(t *testing.T) {
	st := openState(t)
	var recs []Record
	for _, l := range fixtureLines(t) {
		if r, err := Parse(l); err == nil {
			recs = append(recs, r)
		}
	}
	Index(st, recs)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	d, err := ComputeDay(st, "2026-11-10", recs, map[string]any{"test": true}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Active.Card() != 5 || d.Persons.Card() != 1 || d.Dropped["dup"] != 1 {
		t.Fatalf("active %d persons %d dropped %v", d.Active.Card(), d.Persons.Card(), d.Dropped)
	}
	if d.Test == nil || d.Test.Active.Card() != 1 || d.Test.Events["episode_viewed"].N != 1 {
		t.Fatalf("test part: %+v", d.Test)
	}
	if e := d.Events["episode_viewed"]; e.N != 2 || e.U.Card() != 1 || e.By["episode_number"]["1"].N != 1 || e.By["pf"]["android"].N != 2 {
		t.Fatalf("episode_viewed: %+v", e)
	}
	in := d.Installs
	if in.Get(KindInstall).N != 2 || in.Get(KindReinstall).N != 1 || in.Get(KindFirstVisit).N != 1 || in.Suspect != 1 {
		t.Fatalf("installs %+v", in)
	}
	if in.Get(KindInstall).By["source"]["apple_search_ads"].N != 1 || in.Get(KindInstall).By["cc"]["DE"].N != 1 {
		t.Fatalf("install dims %+v", in.Get(KindInstall).By)
	}
	if d.Sessions.N != 4 || d.Sessions.Ended != 1 || d.Sessions.DurP50 != 600000 || d.Sessions.Hist[4] != 1 {
		t.Fatalf("sessions %+v", d.Sessions)
	}
	if d.First.Card() != 5 {
		t.Fatalf("first %d", d.First.Card())
	}
	if d.ActiveBy["pf"]["ios"].U.Card() != 2 || d.ActiveBy["av"]["2.3.1"] == nil {
		t.Fatalf("active by %+v", d.ActiveBy)
	}
	// The subscription of the 12th has a group.
	d12, _ := ComputeDay(st, "2026-11-12", recs, map[string]any{"test": true}, nil, now)
	if d12.Groups["library"].Card() != 1 {
		t.Fatalf("groups %+v", d12.Groups)
	}
	// account_deleted is passive: the deleted test account is not active on the 12th.
	if d12.Test != nil && d12.Test.Active.Card() != 0 {
		t.Fatal("a passive event made the actor active")
	}

	// Save, load, and no change when computed again.
	dir := t.TempDir()
	if ch, err := SaveRollup(dir, d); err != nil || !ch {
		t.Fatal(ch, err)
	}
	again, _ := ComputeDay(st, "2026-11-10", recs, map[string]any{"test": true}, nil, now.Add(time.Hour))
	if ch, _ := SaveRollup(dir, again); ch {
		t.Fatal("the same content counted as a change")
	}
	back, err := LoadRollup(dir, "2026-11-10")
	if err != nil || back.Active.Card() != 5 || back.Events["episode_viewed"].By["episode_number"]["2"].U.Card() != 1 {
		t.Fatalf("load: %+v %v", back, err)
	}
	if RollupDays(dir)[0] != "2026-11-10" {
		t.Fatal(RollupDays(dir))
	}
	if r, err := LoadRollup(dir, "2020-01-01"); r != nil || err != nil {
		t.Fatal(r, err)
	}
}

func TestDimensionCap(t *testing.T) {
	st := openState(t)
	var recs []Record
	for i := 0; i < 150; i++ {
		n := 1
		if i < 5 {
			n = 3 // the largest values survive
		}
		for j := 0; j < n; j++ {
			recs = append(recs, mustParse(t, fmt.Sprintf(`{"$":"analytics","v":1,"id":"%d-%d","ts":"2026-11-10T09:00:00Z","e":"search_performed","d":"d%d","dm":{"query_kind":"k%03d"}}`, i, j, i, i)))
		}
	}
	d, err := ComputeDay(st, "2026-11-10", recs, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	by := d.Events["search_performed"].By["query_kind"]
	if len(by) != MaxDimValues+1 || by[OtherValue].N != 50 || by[OtherValue].U.Card() != 50 || by["k000"].N != 3 {
		t.Fatalf("%d values, other %+v", len(by), by[OtherValue])
	}
}

func TestBitmapJSON(t *testing.T) {
	b := newBitmap()
	for _, x := range []uint32{1, 5, 1 << 20} {
		b.Add(x)
	}
	raw, err := b.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var c Bitmap
	if err := c.UnmarshalJSON(raw); err != nil || c.Card() != 3 || !c.Contains(1<<20) {
		t.Fatal(err, c.Card())
	}
	var nilB *Bitmap
	if nilB.Card() != 0 || nilB.Contains(1) {
		t.Fatal("nil bitmap")
	}
	if raw, _ := nilB.MarshalJSON(); string(raw) != `""` {
		t.Fatal(string(raw))
	}
	var e Bitmap
	if err := e.UnmarshalJSON([]byte(`""`)); err != nil || e.Card() != 0 {
		t.Fatal(err)
	}
	if err := e.UnmarshalJSON([]byte(`"!!"`)); err == nil {
		t.Fatal("bad base64")
	}
	x := or(b, nil)
	x.AndNot(b)
	if x.Card() != 0 {
		t.Fatal(x.Card())
	}
	y := newBitmap()
	y.And(nil)
	n := 0
	b.Each(func(uint32) { n++ })
	if n != 3 {
		t.Fatal(n)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	st := openState(t)
	var recs []Record
	for _, l := range fixtureLines(t) {
		if r, err := Parse(l); err == nil {
			recs = append(recs, r)
		}
	}
	Index(st, recs)
	var buf strings.Builder
	if err := st.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	st2 := openState(t)
	if err := st2.ReadSnapshot(strings.NewReader(buf.String())); err != nil {
		t.Fatal(err)
	}
	st2.View(func(tx *Tx) error {
		a, ok := tx.Actor("u:870")
		b, ok2 := tx.ActorByID(a.ID)
		if !ok || !ok2 || b.Key != "u:870" || tx.Link("5d1c0000-0000-4000-8000-0000000000a1") != "870" {
			t.Errorf("restored: %+v %+v", a, b)
		}
		if k, ok := tx.InstallKey("9f0000000000000000000000000000a1"); !ok || k.N != 2 {
			t.Errorf("install key %+v", k)
		}
		return nil
	})
	// A new actor gets a number after the restored ones.
	st2.Update(func(tx *Tx) error {
		a, _ := tx.EnsureActor("d:new", nil)
		old, _ := tx.Actor("u:870")
		if a.ID <= old.ID {
			t.Errorf("new number %d not after %d", a.ID, old.ID)
		}
		return nil
	})
}

// writer.ndjson is a byte-identical copy of the fixture @veksa-private/analytics-server writes
// (its test fixture records.ndjson): every line must parse and
// validate, and the indexer must read it as the writer means it.
func TestWriterFixture(t *testing.T) {
	f, err := os.Open("testdata/writer.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		r, err := Parse(sc.Text())
		if err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		if ps := Validate(&r); len(ps) > 0 {
			t.Errorf("line %d (%s): %v", n, r.E, ps)
		}
	}
	if n != 21 {
		t.Fatalf("%d lines", n)
	}
	rep, err := CheckFiles([]string{"testdata/writer.ndjson"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Invalid != 0 || rep.Warnings != 0 || rep.Installs[KindInstall] != 1 || rep.Installs[KindReinstall] != 2 || rep.Installs[KindFirstVisit] != 1 || rep.Excluded != 1 {
		t.Fatalf("%+v", rep)
	}
}
