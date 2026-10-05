package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/monitor"
)

// AccessEntry is one nginx access log line (json_combined).
type AccessEntry struct {
	Time    string  `json:"time"`
	IP      string  `json:"ip"`
	Host    string  `json:"host"`
	Method  string  `json:"method"`
	URI     string  `json:"uri"`
	Status  int     `json:"status"`
	Bytes   int64   `json:"bytes"`
	RT      float64 `json:"rt"`
	URT     string  `json:"urt"`
	Ref     string  `json:"ref"`
	UA      string  `json:"ua"`
	Country string  `json:"country"`
}

type TopEntry struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// timeParams parses since/until given as durations ago ("2h") or RFC 3339 times.
func timeParams(r *http.Request) (since, until time.Time, err error) {
	parse := func(v string) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, nil
		}
		d, err := parseAgo(v)
		if err != nil {
			return time.Time{}, fmt.Errorf("bad time %q: use 2h, 30m, 7d or RFC 3339", v)
		}
		return time.Now().Add(-d), nil
	}
	q := r.URL.Query()
	if since, err = parse(q.Get("since")); err != nil {
		return
	}
	until, err = parse(q.Get("until"))
	return
}

func parseAgo(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		return time.Duration(days) * 24 * time.Hour, err
	}
	return time.ParseDuration(v)
}

// grepMatcher accepts a regular expression; grep-style "\|" alternation works too.
func grepMatcher(expr string) (func(string) bool, error) {
	if expr == "" {
		return nil, nil
	}
	re, err := regexp.Compile(strings.ReplaceAll(expr, `\|`, "|"))
	if err != nil {
		return nil, fmt.Errorf("bad --grep: %v", err)
	}
	return re.MatchString, nil
}

type ndjson struct {
	mu      sync.Mutex
	enc     *json.Encoder
	flusher http.Flusher
	follow  bool
}

func newNDJSON(w http.ResponseWriter, follow bool) *ndjson {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	return &ndjson{enc: json.NewEncoder(w), flusher: f, follow: follow}
}

func (n *ndjson) send(v any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.enc.Encode(v)
	if n.follow && n.flusher != nil {
		n.flusher.Flush()
	}
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if s.Logs == nil {
		s.dockerLogs(w, r)
		return
	}
	project, service := r.PathValue("project"), r.PathValue("service")
	if _, err := s.Store.Service(project, service); err != nil {
		writeErr(w, 404, "service not found")
		return
	}
	q := r.URL.Query()
	since, until, err := timeParams(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	grep, err := grepMatcher(q.Get("grep"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var release uint64
	if v := q.Get("release"); v != "" {
		if release, err = strconv.ParseUint(strings.TrimPrefix(v, "r"), 10, 64); err != nil {
			writeErr(w, 400, "bad release")
			return
		}
	}
	tail := 100
	if v := q.Get("tail"); v != "" {
		tail, _ = strconv.Atoi(v)
	}
	if !since.IsZero() && q.Get("tail") == "" {
		tail = 0 // a time range means everything in it
	}
	follow := q.Get("follow") == "1"
	match := func(l logs.Line) bool {
		return (release == 0 || l.R == release) && (grep == nil || grep(l.L))
	}
	key := logs.Key{Project: project, Kind: logs.KindApp, Name: service}
	toLine := func(l logs.Line) LogLine {
		return LogLine{
			Time:      l.Time().UTC().Format(time.RFC3339Nano),
			Container: fmt.Sprintf("%s-%s-r%d-%d", project, service, l.R, l.I),
			Stderr:    l.E,
			Line:      l.L,
		}
	}

	var live <-chan logs.Line
	if follow {
		ch, cancel, err := s.Logs.Subscribe(key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer cancel()
		live = ch
	}
	out := newNDJSON(w, follow)
	if err := s.Logs.Read(key, logs.Query{Since: since, Until: until, Match: match, Tail: tail}, func(l logs.Line) {
		out.send(toLine(l))
	}); err != nil {
		s.Log.Error("read logs", "err", err)
	}
	if !follow {
		return
	}
	out.flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-live:
			if match(l) {
				out.send(toLine(l))
			}
		}
	}
}

// access serves a project's nginx access log: filter, follow, --top summaries, export.
func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	if s.Logs == nil {
		writeErr(w, 501, "access logs need the clowdr log pipeline")
		return
	}
	project := r.PathValue("project")
	q := r.URL.Query()
	since, until, err := timeParams(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	domain := strings.ToLower(q.Get("domain"))
	status := q.Get("status")
	path := q.Get("path")
	match := func(e AccessEntry) bool {
		if domain != "" && strings.ToLower(e.Host) != domain {
			return false
		}
		if path != "" && !strings.HasPrefix(e.URI, path) {
			return false
		}
		if status != "" {
			s := strconv.Itoa(e.Status)
			if strings.HasSuffix(status, "xx") {
				if s[:1] != status[:1] {
					return false
				}
			} else if s != status {
				return false
			}
		}
		return true
	}
	tail := 0
	if v := q.Get("tail"); v != "" {
		tail, _ = strconv.Atoi(v)
	} else if since.IsZero() && q.Get("top") == "" {
		tail = 100
	}
	key := logs.Key{Project: project, Kind: logs.KindAccess, Name: "_"}
	parse := func(l logs.Line) (AccessEntry, bool) {
		var e AccessEntry
		return e, json.Unmarshal([]byte(l.L), &e) == nil
	}

	if top := q.Get("top"); top != "" {
		field := map[string]func(AccessEntry) string{
			"paths":     func(e AccessEntry) string { p, _, _ := strings.Cut(e.URI, "?"); return p },
			"ips":       func(e AccessEntry) string { return e.IP },
			"statuses":  func(e AccessEntry) string { return strconv.Itoa(e.Status) },
			"countries": func(e AccessEntry) string { return e.Country },
			"referers":  func(e AccessEntry) string { return e.Ref },
			"domains":   func(e AccessEntry) string { return e.Host },
		}[top]
		if field == nil {
			writeErr(w, 400, "top: paths|ips|statuses|countries|referers|domains")
			return
		}
		counts := map[string]int{}
		s.Logs.Read(key, logs.Query{Since: since, Until: until}, func(l logs.Line) {
			if e, ok := parse(l); ok && match(e) {
				counts[field(e)]++
			}
		})
		out := make([]TopEntry, 0, len(counts))
		for k, c := range counts {
			out = append(out, TopEntry{Key: k, Count: c})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Key < out[j].Key
		})
		if n, _ := strconv.Atoi(q.Get("limit")); n > 0 && len(out) > n {
			out = out[:n]
		} else if len(out) > 50 {
			out = out[:50]
		}
		writeJSON(w, 200, out)
		return
	}

	if q.Get("export") == "combined" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		s.Logs.Read(key, logs.Query{Since: since, Until: until, Tail: tail}, func(l logs.Line) {
			if e, ok := parse(l); ok && match(e) {
				fmt.Fprintln(w, Combined(e))
			}
		})
		return
	}

	follow := q.Get("follow") == "1"
	var live <-chan logs.Line
	if follow {
		ch, cancel, err := s.Logs.Subscribe(key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer cancel()
		live = ch
	}
	out := newNDJSON(w, follow)
	s.Logs.Read(key, logs.Query{Since: since, Until: until, Tail: tail, Match: func(l logs.Line) bool {
		e, ok := parse(l)
		return ok && match(e)
	}}, func(l logs.Line) {
		e, _ := parse(l)
		out.send(e)
	})
	if !follow {
		return
	}
	out.flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-live:
			if e, ok := parse(l); ok && match(e) {
				out.send(e)
			}
		}
	}
}

// Combined formats an entry in nginx's "combined" format for GoAccess and friends.
func Combined(e AccessEntry) string {
	t, err := time.Parse(time.RFC3339, e.Time)
	ts := e.Time
	if err == nil {
		ts = t.Format("02/Jan/2006:15:04:05 -0700")
	}
	ref, ua := e.Ref, e.UA
	if ref == "" {
		ref = "-"
	}
	if ua == "" {
		ua = "-"
	}
	return fmt.Sprintf(`%s - - [%s] "%s %s HTTP/1.1" %d %d "%s" "%s"`, e.IP, ts, e.Method, e.URI, e.Status, e.Bytes, ref, ua)
}

func (n *ndjson) flush() {
	if n.flusher != nil {
		n.flusher.Flush()
	}
}

// events serves the events journal of a project (admins: project "_platform" for the host).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.Events == nil {
		writeErr(w, 501, "events are not enabled")
		return
	}
	project := r.PathValue("project")
	since, until, err := timeParams(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	tail := 50
	if v := r.URL.Query().Get("tail"); v != "" {
		tail, _ = strconv.Atoi(v)
	} else if !since.IsZero() {
		tail = 0
	}
	follow := r.URL.Query().Get("follow") == "1"
	var live <-chan monitor.Event
	if follow {
		ch, cancel, err := s.Events.Subscribe(project)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer cancel()
		live = ch
	}
	out := newNDJSON(w, follow)
	s.Events.Read(project, logs.Query{Since: since, Until: until, Tail: tail}, func(e monitor.Event) { out.send(e) })
	if !follow {
		return
	}
	out.flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-live:
			out.send(e)
		}
	}
}
