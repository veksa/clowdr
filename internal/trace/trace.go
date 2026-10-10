// Package trace reads the structured trace records services write to stdout (docs/tracing.md):
// one flat record per thing a service did — an inbound request, a model call, an outgoing HTTP
// call, a background job, a log line, an SQL statement — correlated by traceId. clowdrd stores
// them in a service's trace stream; this package parses, rolls up and renders them.
package trace

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Record is one trace record as a service writes it.
type Record struct {
	// Seq is a per-process counter: it orders records that share a millisecond.
	Seq int64 `json:"seq"`
	// Boot identifies the process; Seq restarts on every boot.
	Boot    string `json:"boot,omitempty"`
	TS      string `json:"ts"`
	Type    string `json:"type"`
	TraceID string `json:"traceId"`
	// UserID is whatever the service uses: a number or a string.
	UserID json.RawMessage `json:"userId,omitempty"`
	// Source is what started the trace: "http:GET /orders/12", "job:invoice.send".
	Source string `json:"source,omitempty"`
	// Tags are small facts about the whole trace (a Telegram user id, a tool name).
	Tags       map[string]any `json:"tags,omitempty"`
	Name       string         `json:"name"`
	Level      string         `json:"level,omitempty"`
	DurationMs *float64       `json:"durationMs,omitempty"`
	OK         *bool          `json:"ok,omitempty"`
	CostUSD    float64        `json:"costUsd,omitempty"`
	Data       map[string]any `json:"data,omitempty"`

	// Set by clowdrd from the stream the record came from.
	Service string `json:"service,omitempty"`
	Release uint64 `json:"release,omitempty"`
}

// Orphan is the trace id of records written outside any unit of work (boot, timers).
const Orphan = "-"

// ResponseMark ends the name of the record of an inbound request's response.
const ResponseMark = " ←"

// Parse decodes one stored line. ok is false for anything that is not a trace record.
func Parse(line string) (Record, bool) {
	var r Record
	if json.Unmarshal([]byte(line), &r) != nil || r.TraceID == "" {
		return r, false
	}
	return r, true
}

// Time is the record's timestamp (zero when unparsable).
func (r Record) Time() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, r.TS)
	return t
}

// User renders UserID without JSON quotes; "" when unset.
func (r Record) User() string {
	if len(r.UserID) == 0 || string(r.UserID) == "null" {
		return ""
	}
	if s, err := strconv.Unquote(string(r.UserID)); err == nil {
		return s
	}
	return string(r.UserID)
}

// Failed reports a record that represents something going wrong.
func (r Record) Failed() bool {
	return r.Level == "error" || r.Level == "warn" || (r.OK != nil && !*r.OK)
}

// Sort orders records by time, then by Seq.
func Sort(rs []Record) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].TS != rs[j].TS {
			return rs[i].TS < rs[j].TS
		}
		return rs[i].Seq < rs[j].Seq
	})
}

// Summary is one trace rolled up: what a list of traces shows before one is opened.
type Summary struct {
	TraceID    string         `json:"traceId"`
	Service    string         `json:"service,omitempty"`
	StartedAt  string         `json:"startedAt"`
	EndedAt    string         `json:"endedAt"`
	DurationMs int64          `json:"durationMs"`
	UserID     string         `json:"userId,omitempty"`
	Source     string         `json:"source,omitempty"`
	Tags       map[string]any `json:"tags,omitempty"`
	Records    int            `json:"records"`
	LLMCalls   int            `json:"llmCalls"`
	DBCalls    int            `json:"dbCalls"`
	Errors     int            `json:"errors"`
	CostUSD    float64        `json:"costUsd"`
	Models     []string       `json:"models,omitempty"`
	// Input is what came in: the request or job name, or the first note.
	Input string `json:"input,omitempty"`
	// Output is what went out: the response status or the job outcome.
	Output string `json:"output,omitempty"`
}

// Roller builds a Summary from records fed in time order.
type Roller struct {
	s      Summary
	start  time.Time
	end    time.Time
	models map[string]bool
}

func NewRoller(traceID string) *Roller {
	return &Roller{s: Summary{TraceID: traceID}, models: map[string]bool{}}
}

func (r *Roller) Add(rec Record) {
	s := &r.s
	s.Records++
	if t := rec.Time(); !t.IsZero() {
		if r.start.IsZero() || t.Before(r.start) {
			r.start, s.StartedAt = t, rec.TS
		}
		if t.After(r.end) || r.end.IsZero() {
			r.end, s.EndedAt = t, rec.TS
		}
	}
	if s.Service == "" {
		s.Service = rec.Service
	}
	if s.UserID == "" {
		s.UserID = rec.User()
	}
	if s.Source == "" {
		s.Source = rec.Source
	}
	for k, v := range rec.Tags {
		if s.Tags == nil {
			s.Tags = map[string]any{}
		}
		s.Tags[k] = v
	}
	s.CostUSD += rec.CostUSD
	if rec.Level == "error" || (rec.OK != nil && !*rec.OK) {
		s.Errors++
	}
	switch rec.Type {
	case "llm":
		s.LLMCalls++
		if !r.models[rec.Name] {
			r.models[rec.Name] = true
			s.Models = append(s.Models, rec.Name)
		}
	case "db":
		s.DBCalls++
	case "http":
		if rec.Data["dir"] != "in" {
			break
		}
		// One request in, one response out. The response wins OUT even when work goes on
		// after it: it is what the caller got.
		if name, ok := strings.CutSuffix(rec.Name, ResponseMark); ok {
			s.Output = strings.TrimSpace(str(rec.Data["status"]))
			if s.Output == "" {
				s.Output = name
			}
		} else if s.Input == "" {
			s.Input = rec.Name
		}
	case "job":
		if s.Input == "" {
			s.Input = rec.Name
		} else if o := str(rec.Data["outcome"]); o != "" {
			s.Output = o
		}
	case "note":
		if s.Input == "" {
			s.Input = rec.Name
		}
	}
}

func (r *Roller) Summary() Summary {
	s := r.s
	if !r.start.IsZero() && !r.end.IsZero() {
		s.DurationMs = r.end.Sub(r.start).Milliseconds()
	}
	return s
}

// Summarize rolls up a whole trace.
func Summarize(traceID string, rs []Record) Summary {
	sorted := append([]Record(nil), rs...)
	Sort(sorted)
	r := NewRoller(traceID)
	for _, rec := range sorted {
		r.Add(rec)
	}
	return r.Summary()
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
