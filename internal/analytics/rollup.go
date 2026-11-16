package analytics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// RollupVersion is the version of the rollup file format.
const RollupVersion = 1

// MaxDimValues bounds the values of one dimension in one day; the rest go to OtherValue.
const MaxDimValues = 100

// OtherValue collects the values of a dimension beyond MaxDimValues.
const OtherValue = "$other"

// SessionBounds are the upper bounds (seconds) of the session duration histogram; the last
// bucket is above the last bound.
var SessionBounds = []int64{10, 30, 60, 180, 600, 1800, 3600}

// Cell is a count of events and the set of actors behind them.
type Cell struct {
	N int     `json:"n"`
	U *Bitmap `json:"u,omitempty"`
}

func (c *Cell) add(aid uint32) {
	c.N++
	if c.U == nil {
		c.U = newBitmap()
	}
	c.U.Add(aid)
}

// Dims is dimension → value → cell.
type Dims map[string]map[string]*Cell

func (d Dims) add(dim, val string, aid uint32) {
	if val == "" {
		return
	}
	m := d[dim]
	if m == nil {
		m = map[string]*Cell{}
		d[dim] = m
	}
	c := m[val]
	if c == nil {
		c = &Cell{}
		m[val] = c
	}
	c.add(aid)
}

// trim keeps the MaxDimValues largest values of every dimension and merges the rest into
// OtherValue.
func (d Dims) trim() {
	for dim, m := range d {
		if len(m) <= MaxDimValues {
			continue
		}
		vals := make([]string, 0, len(m))
		for v := range m {
			vals = append(vals, v)
		}
		sort.Slice(vals, func(i, j int) bool {
			if m[vals[i]].N != m[vals[j]].N {
				return m[vals[i]].N > m[vals[j]].N
			}
			return vals[i] < vals[j]
		})
		other := m[OtherValue]
		if other == nil {
			other = &Cell{U: newBitmap()}
		}
		kept := map[string]*Cell{}
		for i, v := range vals {
			if v == OtherValue {
				continue
			}
			if len(kept) < MaxDimValues {
				kept[v] = m[v]
				continue
			}
			_ = i
			other.N += m[v].N
			if other.U == nil {
				other.U = newBitmap()
			}
			other.U.Or(m[v].U)
		}
		kept[OtherValue] = other
		d[dim] = kept
	}
}

// EventAgg is one event in one day.
type EventAgg struct {
	Cell
	By Dims `json:"by,omitempty"`
}

// Sessions summarises the sessions of a day.
type Sessions struct {
	N        int   `json:"n"`
	Ended    int   `json:"ended,omitempty"` // sessions with a $session_end and its duration
	DurP50   int64 `json:"dur_ms_p50,omitempty"`
	DurTotal int64 `json:"dur_ms_total,omitempty"`
	Hist     []int `json:"dur_hist,omitempty"` // by SessionBounds
}

// InstallAgg counts installs of one kind.
type InstallAgg struct {
	Cell
	By Dims `json:"by,omitempty"`
}

// Installs counts the $first_open records of a day by what they were.
type Installs struct {
	Install    *InstallAgg `json:"install,omitempty"`
	Reinstall  *InstallAgg `json:"reinstall,omitempty"`
	FirstVisit *InstallAgg `json:"first_visit,omitempty"`
	Suspect    int         `json:"suspect,omitempty"`
}

// InstallDims are what installs are broken down by.
var InstallDims = []string{"source", "campaign", "store", "cc", "pf", "app"}

func (in *Installs) kind(k string) **InstallAgg {
	switch k {
	case KindInstall:
		return &in.Install
	case KindReinstall:
		return &in.Reinstall
	}
	return &in.FirstVisit
}

// Get returns the counts of one kind (never nil).
func (in *Installs) Get(k string) *InstallAgg {
	if in == nil {
		return &InstallAgg{}
	}
	if a := *in.kind(k); a != nil {
		return a
	}
	return &InstallAgg{}
}

// Part is everything counted for one set of actors in a day: the main part leaves out the
// excluded persons (test accounts), which are counted in their own part.
type Part struct {
	Active   *Bitmap              `json:"active,omitempty"`
	First    *Bitmap              `json:"first,omitempty"`
	Persons  *Bitmap              `json:"persons,omitempty"`
	ActiveBy Dims                 `json:"active_by,omitempty"`
	Events   map[string]*EventAgg `json:"events,omitempty"`
	Sessions *Sessions            `json:"sessions,omitempty"`
	Installs *Installs            `json:"installs,omitempty"`
	Groups   map[string]*Bitmap   `json:"groups,omitempty"`

	durs     []int64
	sessions map[string]bool
}

func newPart() *Part {
	return &Part{Active: newBitmap(), First: newBitmap(), Persons: newBitmap(), ActiveBy: Dims{},
		Events: map[string]*EventAgg{}, Sessions: &Sessions{Hist: make([]int, len(SessionBounds)+1)},
		Installs: &Installs{}, Groups: map[string]*Bitmap{}, sessions: map[string]bool{}}
}

// DayRollup is one day (by event time, UTC) of a project.
type DayRollup struct {
	V   int    `json:"v"`
	Day string `json:"day"`
	Part
	Test     *Part          `json:"test,omitempty"`
	Dropped  map[string]int `json:"dropped,omitempty"`
	Computed string         `json:"computed,omitempty"`
}

// Parts returns the main part, and the test part too when includeTest is set.
func (d *DayRollup) Parts(includeTest bool) []*Part {
	if includeTest && d.Test != nil {
		return []*Part{&d.Part, d.Test}
	}
	return []*Part{&d.Part}
}

// eventCount is the number of events the rollup counts, test persons included.
func (d *DayRollup) eventCount() int {
	n := 0
	for _, p := range d.Parts(true) {
		for _, e := range p.Events {
			n += e.N
		}
	}
	return n
}

// resolved is a record with its actor.
type resolved struct {
	r        *Record
	aid      uint32
	person   bool
	excluded bool
	install  string
}

// ComputeDay builds the rollup of one day from its records (any order, duplicates allowed). It
// numbers new actors in the state; exclude is the rule for persons counted apart.
func ComputeDay(st *State, day string, recs []Record, exclude map[string]any, now time.Time) (*DayRollup, error) {
	out := &DayRollup{V: RollupVersion, Day: day, Part: *newPart(), Dropped: map[string]int{}, Computed: now.UTC().Format(time.RFC3339)}
	seen := make(map[string]bool, len(recs))
	uniq := make([]*Record, 0, len(recs))
	for i := range recs {
		r := &recs[i]
		if r.Day() != day {
			continue
		}
		if seen[r.ID] {
			out.Dropped["dup"]++
			continue
		}
		seen[r.ID] = true
		uniq = append(uniq, r)
	}
	sort.SliceStable(uniq, func(i, j int) bool { return uniq[i].t.Before(uniq[j].t) })

	res := make([]resolved, 0, len(uniq))
	groupIDs := map[string]uint32{}
	err := st.Update(func(t *Tx) error {
		actors := map[string]Actor{}
		excluded := map[string]bool{}
		for _, r := range uniq {
			key := t.ActorKey(r)
			if key == "" {
				out.Dropped["no_actor"]++
				continue
			}
			a, err := t.EnsureActor(key, r)
			if err != nil {
				return err
			}
			actors[key] = a
			ex, ok := excluded[key]
			if !ok {
				if len(key) > 2 && key[:2] == "u:" {
					p, _ := t.Person(key[2:])
					ex = p.matches(exclude)
				}
				excluded[key] = ex
			}
			x := resolved{r: r, aid: a.ID, person: key[:2] == "u:", excluded: ex}
			if r.E == EventFirstOpen {
				x.install, _ = t.installKind(r)
			}
			for typ, gk := range r.G {
				id, err := t.GroupID(typ, str(gk))
				if err != nil {
					return err
				}
				groupIDs[typ+"\x00"+str(gk)] = id
			}
			res = append(res, x)
		}
		// first: actors whose first day is this one (read after numbering, so a late event
		// that moved the first day back counts here).
		for key, a := range actors {
			a2, _ := t.Actor(key)
			if a2.FirstDay == day {
				p := &out.Part
				if excluded[key] {
					if out.Test == nil {
						out.Test = newPart()
					}
					p = out.Test
				}
				p.First.Add(a.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, x := range res {
		p := &out.Part
		if x.excluded {
			if out.Test == nil {
				out.Test = newPart()
			}
			p = out.Test
		}
		p.add(x, groupIDs)
	}
	out.finish()
	if out.Test != nil {
		out.Test.finishPart()
	}
	if len(out.Dropped) == 0 {
		out.Dropped = nil
	}
	return out, nil
}

func (p *Part) add(x resolved, groupIDs map[string]uint32) {
	r := x.r
	if !passive[r.E] {
		p.Active.Add(x.aid)
		if x.person {
			p.Persons.Add(x.aid)
		}
		for _, d := range BuiltinDims {
			p.ActiveBy.add(d, r.Dim(d), x.aid)
		}
	}
	e := p.Events[r.E]
	if e == nil {
		e = &EventAgg{By: Dims{}}
		p.Events[r.E] = e
	}
	e.add(x.aid)
	for _, d := range BuiltinDims {
		e.By.add(d, r.Dim(d), x.aid)
	}
	for k, v := range r.DM {
		if k == "suspect" {
			continue
		}
		e.By.add(k, str(v), x.aid)
	}
	if r.S != "" && !passive[r.E] {
		p.sessions[r.S] = true
	}
	if r.E == EventSessionEnd {
		if v, ok := num(r.P["duration_ms"]); ok && v >= 0 {
			p.durs = append(p.durs, int64(v))
		}
	}
	if r.E == EventFirstOpen {
		if r.suspect() {
			p.Installs.Suspect++
		} else {
			k := p.Installs.kind(x.install)
			if *k == nil {
				*k = &InstallAgg{By: Dims{}}
			}
			(*k).add(x.aid)
			for _, d := range InstallDims {
				v := str(r.DM[d])
				switch d {
				case "source":
					v = sourceOf(r)
				case "cc", "pf", "app":
					v = r.Dim(d)
				}
				(*k).By.add(d, v, x.aid)
			}
		}
	}
	for typ, gk := range r.G {
		b := p.Groups[typ]
		if b == nil {
			b = newBitmap()
			p.Groups[typ] = b
		}
		b.Add(groupIDs[typ+"\x00"+str(gk)])
	}
}

func (d *DayRollup) finish() { d.Part.finishPart() }

func (p *Part) finishPart() {
	p.ActiveBy.trim()
	for _, e := range p.Events {
		e.By.trim()
		if len(e.By) == 0 {
			e.By = nil
		}
	}
	for _, k := range []*InstallAgg{p.Installs.Install, p.Installs.Reinstall, p.Installs.FirstVisit} {
		if k != nil {
			k.By.trim()
		}
	}
	s := p.Sessions
	s.N = len(p.sessions)
	s.Ended = len(p.durs)
	if len(p.durs) > 0 {
		sort.Slice(p.durs, func(i, j int) bool { return p.durs[i] < p.durs[j] })
		s.DurP50 = p.durs[(len(p.durs)-1)/2]
		for _, v := range p.durs {
			s.DurTotal += v
			s.Hist[histBucket(v)]++
		}
	} else {
		s.Hist = nil
	}
	if s.N == 0 && s.Ended == 0 {
		p.Sessions = nil
	}
	if p.Installs.Install == nil && p.Installs.Reinstall == nil && p.Installs.FirstVisit == nil && p.Installs.Suspect == 0 {
		p.Installs = nil
	}
	if len(p.Groups) == 0 {
		p.Groups = nil
	}
	if len(p.ActiveBy) == 0 {
		p.ActiveBy = nil
	}
}

func histBucket(ms int64) int {
	for i, b := range SessionBounds {
		if ms <= b*1000 {
			return i
		}
	}
	return len(SessionBounds)
}

// ─── Files ───────────────────────────────────────────────────────────────────

func rollupPath(dir, day string) string { return filepath.Join(dir, "days", day+".roll.zst") }

var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(1))
	zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	zmu     sync.Mutex
)

func compress(b []byte) []byte {
	zmu.Lock()
	defer zmu.Unlock()
	return zenc.EncodeAll(b, nil)
}

func decompress(b []byte) ([]byte, error) { return zdec.DecodeAll(b, nil) }

// encodeRollup serialises a rollup as zstd JSON.
func encodeRollup(d *DayRollup) ([]byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return compress(raw), nil
}

// SaveRollup writes a rollup file atomically and reports whether its content changed.
func SaveRollup(dir string, d *DayRollup) (bool, error) {
	cmp := *d
	cmp.Computed = ""
	body, err := json.Marshal(&cmp)
	if err != nil {
		return false, err
	}
	path := rollupPath(dir, d.Day)
	if old, err := LoadRollup(dir, d.Day); err == nil && old != nil {
		old.Computed = ""
		if ob, err := json.Marshal(old); err == nil && bytes.Equal(ob, body) {
			return false, nil
		}
	}
	b, err := encodeRollup(d)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// LoadRollup reads a rollup; nil without error when the day has none.
func LoadRollup(dir, day string) (*DayRollup, error) {
	b, err := os.ReadFile(rollupPath(dir, day))
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return decodeRollup(b)
}

func decodeRollup(b []byte) (*DayRollup, error) {
	raw, err := decompress(b)
	if err != nil {
		return nil, err
	}
	var d DayRollup
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// RollupDays lists the days with a rollup file, sorted.
func RollupDays(dir string) []string {
	es, _ := os.ReadDir(filepath.Join(dir, "days"))
	var out []string
	for _, e := range es {
		if n := e.Name(); len(n) == len("2006-01-02.roll.zst") && filepath.Ext(n) == ".zst" {
			out = append(out, n[:10])
		}
	}
	sort.Strings(out)
	return out
}
