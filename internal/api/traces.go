package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/trace"
)

// TraceDetail is one trace in full.
type TraceDetail struct {
	Summary trace.Summary  `json:"summary"`
	Records []trace.Record `json:"records"`
}

// traceStreams returns the trace streams to read: one service, or every service of the project.
func (s *Server) traceStreams(w http.ResponseWriter, r *http.Request) ([]logs.Key, bool) {
	if s.Logs == nil {
		writeErr(w, 501, "traces need the clowdr log pipeline")
		return nil, false
	}
	project := r.PathValue("project")
	var names []string
	if svc := r.URL.Query().Get("service"); svc != "" {
		if _, err := s.Store.Service(project, svc); err != nil {
			writeErr(w, 404, "service not found")
			return nil, false
		}
		names = []string{svc}
	} else {
		ss, err := s.Store.Services(project)
		if err != nil {
			writeErr(w, 500, err.Error())
			return nil, false
		}
		for _, sv := range ss {
			names = append(names, sv.Name)
		}
	}
	keys := make([]logs.Key, len(names))
	for i, n := range names {
		keys[i] = logs.Key{Project: project, Kind: logs.KindTrace, Name: n}
	}
	return keys, true
}

func traceFilter(r *http.Request) trace.Filter {
	q := r.URL.Query()
	split := func(v string) []string {
		if v == "" {
			return nil
		}
		return strings.Split(v, ",")
	}
	return trace.Filter{
		Q:        q.Get("q"),
		TraceID:  q.Get("trace"),
		User:     q.Get("user"),
		Source:   q.Get("source"),
		Types:    split(q.Get("type")),
		Levels:   split(q.Get("level")),
		Failures: q.Get("failures") == "1",
	}
}

// window is the time range of a trace query; since defaults to def ago.
func window(w http.ResponseWriter, r *http.Request, def time.Duration) (since, until time.Time, ok bool) {
	since, until, err := timeParams(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return since, until, false
	}
	if since.IsZero() {
		since = time.Now().Add(-def)
	}
	return since, until, true
}

func toRecord(k logs.Key, l logs.Line) (trace.Record, bool) {
	rec, ok := trace.Parse(l.L)
	rec.Service, rec.Release = k.Name, l.R
	return rec, ok
}

// traces lists traces rolled up: GET /v1/projects/{project}/traces.
func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	keys, ok := s.traceStreams(w, r)
	if !ok {
		return
	}
	since, until, ok := window(w, r, 24*time.Hour)
	if !ok {
		return
	}
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	f := traceFilter(r)
	l := &trace.Lister{Filter: trace.Filter{Q: f.Q, User: f.User, Source: f.Source}, ErrorsOnly: r.URL.Query().Get("errors") == "1"}
	for _, k := range keys {
		s.Logs.Read(k, logs.Query{Since: since, Until: until}, func(line logs.Line) {
			if rec, ok := toRecord(k, line); ok {
				l.Add(line.L, rec)
			}
		})
	}
	writeJSON(w, 200, l.Result(limit))
}

// traceOne returns one trace in full: GET /v1/projects/{project}/traces/{trace}.
func (s *Server) traceOne(w http.ResponseWriter, r *http.Request) {
	keys, ok := s.traceStreams(w, r)
	if !ok {
		return
	}
	since, until, ok := window(w, r, 7*24*time.Hour)
	if !ok {
		return
	}
	id := r.PathValue("trace")
	f := trace.Filter{TraceID: id}
	var rs []trace.Record
	for _, k := range keys {
		s.Logs.Read(k, logs.Query{Since: since, Until: until, Match: func(l logs.Line) bool { return f.Prefilter(l.L) }}, func(line logs.Line) {
			if rec, ok := toRecord(k, line); ok && rec.TraceID == id {
				rs = append(rs, rec)
			}
		})
	}
	if len(rs) == 0 {
		writeErr(w, 404, "trace not found (widen --since?)")
		return
	}
	trace.Sort(rs)
	writeJSON(w, 200, TraceDetail{Summary: trace.Summarize(id, rs), Records: rs})
}

// traceRecords streams raw records: GET /v1/projects/{project}/trace-records.
func (s *Server) traceRecords(w http.ResponseWriter, r *http.Request) {
	keys, ok := s.traceStreams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	since, until, err := timeParams(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	tail := 100
	if v := q.Get("tail"); v != "" {
		tail, _ = strconv.Atoi(v)
	} else if !since.IsZero() {
		tail = 0
	}
	f := traceFilter(r)
	match := func(k logs.Key, l logs.Line) (trace.Record, bool) {
		if !f.Prefilter(l.L) {
			return trace.Record{}, false
		}
		rec, ok := toRecord(k, l)
		return rec, ok && f.Match(rec)
	}

	follow := q.Get("follow") == "1"
	type tagged struct {
		k logs.Key
		l logs.Line
	}
	live := make(chan tagged, 1024)
	if follow {
		for _, k := range keys {
			ch, cancel, err := s.Logs.Subscribe(k)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			defer cancel()
			go func() {
				for {
					select {
					case <-r.Context().Done():
						return
					case l := <-ch:
						select {
						case live <- tagged{k, l}:
						default:
						}
					}
				}
			}()
		}
	}

	// Every stream's matches, merged in time order and cut to the tail.
	var all []trace.Record
	for _, k := range keys {
		s.Logs.Read(k, logs.Query{Since: since, Until: until, Tail: tail, Match: func(l logs.Line) bool {
			_, ok := match(k, l)
			return ok
		}}, func(l logs.Line) {
			if rec, ok := toRecord(k, l); ok {
				all = append(all, rec)
			}
		})
	}
	slices.SortStableFunc(all, func(a, b trace.Record) int { return strings.Compare(a.TS, b.TS) })
	if tail > 0 && len(all) > tail {
		all = all[len(all)-tail:]
	}
	out := newNDJSON(w, follow)
	for _, rec := range all {
		out.send(rec)
	}
	if !follow {
		return
	}
	out.flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-live:
			if rec, ok := match(t.k, t.l); ok {
				out.send(rec)
			}
		}
	}
}
