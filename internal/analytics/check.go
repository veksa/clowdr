package analytics

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CheckReport is what `clowctl analytics check` prints: whether the lines a service wrote are
// valid records, and what clowdr would count from them.
type CheckReport struct {
	Lines    int                   `json:"lines"`
	Records  int                   `json:"records"`
	Other    int                   `json:"other_lines"` // lines without an analytics record
	Invalid  int                   `json:"invalid"`     // records clowdr cannot count
	Warnings int                   `json:"warnings"`    // records clowdr counts, with problems
	Problems []string              `json:"problems,omitempty"`
	Dup      int                   `json:"dup"`
	Days     []string              `json:"days,omitempty"`
	Events   map[string]CheckEvent `json:"events"`
	Actors   int                   `json:"actors"`
	Persons  int                   `json:"persons"`
	// Merged counts anonymous events (device only) that count for a person through a link.
	Merged   int            `json:"merged"`
	Installs map[string]int `json:"installs"` // install, reinstall, first_visit, suspect
	Sessions int            `json:"sessions"`
	Excluded int            `json:"excluded"` // actors left out as test persons
}

// CheckEvent is one event in a CheckReport.
type CheckEvent struct {
	N      int `json:"n"`
	Actors int `json:"actors"`
}

const maxProblems = 30

// Check reads files of lines (a service's stdout, a product's local analytics file, or
// `clowctl logs` output: the record may follow a prefix), in order, as clowdr would receive
// them, and counts them with a throwaway state.
func Check(readers []io.Reader, names []string) (*CheckReport, error) {
	rep := &CheckReport{Events: map[string]CheckEvent{}, Installs: map[string]int{}}
	var recs []Record
	problem := func(where, msg string) {
		if len(rep.Problems) < maxProblems {
			rep.Problems = append(rep.Problems, where+": "+msg)
		}
	}
	for fi, rd := range readers {
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		n := 0
		for sc.Scan() {
			n++
			rep.Lines++
			line := sc.Text()
			i := strings.Index(line, Mark)
			if i < 0 {
				rep.Other++
				continue
			}
			rep.Records++
			where := fmt.Sprintf("%s:%d", names[fi], n)
			r, err := Parse(line[i:])
			if err != nil {
				rep.Invalid++
				problem(where, err.Error())
				continue
			}
			if ps := Validate(&r); len(ps) > 0 {
				rep.Warnings++
				problem(where, strings.Join(ps, "; "))
			}
			if len(line)-i > 8<<10 {
				rep.Warnings++
				problem(where, "longer than 8 KB")
			}
			recs = append(recs, r)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", names[fi], err)
		}
	}

	dir, err := os.MkdirTemp("", "clowdr-analytics-check-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	st, err := OpenState(filepath.Join(dir, "state.db"))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	// Index in order, as the indexer would, one record per batch so an install key seen in an
	// earlier file decides a later install.
	for i := range recs {
		if _, err := Index(st, recs[i:i+1]); err != nil {
			return nil, err
		}
	}
	byDay := map[string][]Record{}
	for _, r := range recs {
		byDay[r.Day()] = append(byDay[r.Day()], r)
	}
	for d := range byDay {
		rep.Days = append(rep.Days, d)
	}
	sort.Strings(rep.Days)
	actors, persons, excluded := newBitmap(), newBitmap(), newBitmap()
	evActors := map[string]*Bitmap{}
	exclude := map[string]any{"test": true}
	for _, d := range rep.Days {
		roll, err := ComputeDay(st, d, byDay[d], exclude, time.Now())
		if err != nil {
			return nil, err
		}
		rep.Dup += roll.Dropped["dup"]
		for pi, part := range roll.Parts(true) {
			if pi == 1 {
				excluded.Or(part.Active)
				excluded.Or(part.First)
			}
			actors.Or(part.Active)
			persons.Or(part.Persons)
			for e, agg := range part.Events {
				ev := rep.Events[e]
				ev.N += agg.N
				rep.Events[e] = ev
				if evActors[e] == nil {
					evActors[e] = newBitmap()
				}
				evActors[e].Or(agg.U)
			}
			if part.Sessions != nil {
				rep.Sessions += part.Sessions.N
			}
			if in := part.Installs; in != nil {
				rep.Installs[KindInstall] += in.Get(KindInstall).N
				rep.Installs[KindReinstall] += in.Get(KindReinstall).N
				rep.Installs[KindFirstVisit] += in.Get(KindFirstVisit).N
				rep.Installs["suspect"] += in.Suspect
			}
		}
	}
	for e, b := range evActors {
		ev := rep.Events[e]
		ev.Actors = b.Card()
		rep.Events[e] = ev
	}
	rep.Actors, rep.Persons, rep.Excluded = actors.Card(), persons.Card(), excluded.Card()
	st.View(func(t *Tx) error {
		for i := range recs {
			r := &recs[i]
			if r.User() == "" && r.D != "" && t.Link(r.D) != "" {
				rep.Merged++
			}
		}
		return nil
	})
	return rep, nil
}

// CheckFiles runs Check over files by path ("-" is stdin); a file compressed with zstd is read
// uncompressed.
func CheckFiles(paths []string) (*CheckReport, error) {
	var rs []io.Reader
	for _, p := range paths {
		if p == "-" {
			rs = append(rs, os.Stdin)
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		rc, err := RecordsReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		defer rc.Close()
		rs = append(rs, rc)
	}
	return Check(rs, paths)
}
