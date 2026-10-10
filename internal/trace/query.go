package trace

import (
	"slices"
	"sort"
	"strings"
)

// Filter selects records. Empty fields match everything.
type Filter struct {
	// Q is a case-insensitive substring of the whole record (name, source, data).
	Q       string
	TraceID string
	User    string
	// Source is a substring of the record's source ("studio:" matches every studio job).
	Source   string
	Types    []string
	Levels   []string
	Failures bool // only error/warn records and failed calls
}

// Prefilter is a cheap check on the raw line before it is decoded.
func (f Filter) Prefilter(line string) bool {
	if f.TraceID != "" && !strings.Contains(line, `"traceId":"`+f.TraceID+`"`) {
		return false
	}
	return f.Q == "" || containsFold(line, f.Q)
}

// Match checks a decoded record against every field except Q (see Prefilter).
func (f Filter) Match(r Record) bool {
	switch {
	case f.TraceID != "" && r.TraceID != f.TraceID,
		f.User != "" && r.User() != f.User,
		f.Source != "" && !strings.Contains(r.Source, f.Source),
		len(f.Types) > 0 && !slices.Contains(f.Types, r.Type),
		len(f.Levels) > 0 && !slices.Contains(f.Levels, r.Level),
		f.Failures && !r.Failed():
		return false
	}
	return true
}

// Lister rolls records up into traces. A trace is kept when any of its records matches Q,
// User and Source: filtering per record would summarise half a trace.
type Lister struct {
	Filter     Filter
	ErrorsOnly bool

	rollers map[string]*Roller
	matched map[string]bool
}

func (l *Lister) Add(line string, r Record) {
	if r.TraceID == Orphan {
		return
	}
	if l.rollers == nil {
		l.rollers, l.matched = map[string]*Roller{}, map[string]bool{}
	}
	ro := l.rollers[r.TraceID]
	if ro == nil {
		ro = NewRoller(r.TraceID)
		l.rollers[r.TraceID] = ro
	}
	ro.Add(r)
	if !l.matched[r.TraceID] && (l.Filter.Q == "" || containsFold(line, l.Filter.Q)) &&
		(l.Filter.User == "" || r.User() == l.Filter.User) &&
		(l.Filter.Source == "" || strings.Contains(r.Source, l.Filter.Source)) {
		l.matched[r.TraceID] = true
	}
}

// Result returns the matching traces, newest first, at most limit (0 = all).
func (l *Lister) Result(limit int) []Summary {
	out := []Summary{}
	for id, ro := range l.rollers {
		if !l.matched[id] {
			continue
		}
		s := ro.Summary()
		if l.ErrorsOnly && s.Errors == 0 {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
