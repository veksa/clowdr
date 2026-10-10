package trace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// FormatOptions control the plain-text rendering.
type FormatOptions struct {
	// MaxChars is the per-field budget; a longer field is cut with an explicit marker.
	MaxChars int
	// Verbose shows every message of a model call (otherwise the last 4).
	Verbose bool
}

const pad10 = "          "

// FormatTrace renders one trace top to bottom: what came in, which model saw which prompt and
// what it answered, what went out and what came back. Nothing is elided except over-long
// fields, and every cut is marked.
func FormatTrace(s Summary, rs []Record, o FormatOptions) string {
	if o.MaxChars == 0 {
		o.MaxChars = 4000
	}
	var b strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	line("═══ TRACE %s %s", s.TraceID, strings.Repeat("═", max(0, 50-len(s.TraceID))))
	line("time    %s → %s  (%dms)", s.StartedAt, s.EndedAt, s.DurationMs)
	who := "who     user=" + orQ(s.UserID) + " source=" + orQ(s.Source)
	if s.Service != "" {
		who += " service=" + s.Service
	}
	line("%s", who)
	line("totals  records=%d llm=%d sql=%d errors=%d cost=$%.6f", s.Records, s.LLMCalls, s.DBCalls, s.Errors, s.CostUSD)
	if len(s.Models) > 0 {
		line("models  %s", strings.Join(s.Models, ", "))
	}
	if s.Input != "" {
		line("IN      %s", oneLine(s.Input, 300))
	}
	if s.Output != "" {
		line("OUT     %s", oneLine(s.Output, 300))
	}
	line("%s", strings.Repeat("─", 60))
	start, _ := time.Parse(time.RFC3339Nano, s.StartedAt)
	for _, r := range rs {
		for _, l := range formatRecord(r, o, start) {
			line("%s", l)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatRecords renders records grouped by trace, each with its clock time.
func FormatRecords(rs []Record, o FormatOptions) string {
	if len(rs) == 0 {
		return "(no records)"
	}
	if o.MaxChars == 0 {
		o.MaxChars = 1500
	}
	var out []string
	last := ""
	for _, r := range rs {
		if r.TraceID != last {
			h := "── trace " + r.TraceID
			if u := r.User(); u != "" {
				h += " user=" + u
			}
			if r.Service != "" {
				h += " service=" + r.Service
			}
			out = append(out, h)
			last = r.TraceID
		}
		out = append(out, formatRecord(r, o, time.Time{})...)
	}
	return strings.Join(out, "\n")
}

// FormatSummaries renders a list of traces, one per line.
func FormatSummaries(ss []Summary) string {
	if len(ss) == 0 {
		return "(no traces)"
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		errs, cost, in, outp := "", "", s.Input, ""
		if s.Errors > 0 {
			errs = fmt.Sprintf("ERR=%d", s.Errors)
		}
		if s.CostUSD > 0 {
			cost = fmt.Sprintf("$%.5f", s.CostUSD)
		}
		if in == "" {
			in = s.Source
		}
		if s.Output != "" {
			outp = "→ " + oneLine(s.Output, 60)
		}
		out = append(out, strings.TrimRight(strings.Join([]string{
			s.StartedAt,
			pad(s.TraceID, 16),
			pad("user="+orQ(s.UserID), 12),
			pad(fmt.Sprintf("%dms", s.DurationMs), 8),
			pad(fmt.Sprintf("llm=%d", s.LLMCalls), 6),
			pad(errs, 7),
			pad(cost, 10),
			oneLine(in, 60),
			outp,
		}, " "), " "))
	}
	return strings.Join(out, "\n")
}

func formatRecord(r Record, o FormatOptions, start time.Time) []string {
	offset := r.TS
	if len(offset) >= 23 {
		offset = offset[11:23]
	}
	if !start.IsZero() {
		offset = fmt.Sprintf("+%dms", r.Time().Sub(start).Milliseconds())
	}
	var meta []string
	if r.DurationMs != nil {
		meta = append(meta, fmt.Sprintf("%.0fms", *r.DurationMs))
	}
	if r.OK != nil && !*r.OK {
		meta = append(meta, "FAILED")
	}
	if r.Level != "" && r.Level != "info" {
		meta = append(meta, r.Level)
	}
	if r.CostUSD != 0 {
		meta = append(meta, fmt.Sprintf("$%.6f", r.CostUSD))
	}
	head := pad(offset, 9) + " " + pad(r.Type, 9) + " " + r.Name
	if len(meta) > 0 {
		head += "  [" + strings.Join(meta, " ") + "]"
	}
	lines := []string{head}
	d := r.Data
	if d == nil {
		d = map[string]any{}
	}
	switch r.Type {
	case "llm":
		lines = append(lines, formatLLM(d, o)...)
	case "log":
		lines = append(lines, field("", d["message"], o.MaxChars)...)
	case "http":
		url := d["path"]
		if url == nil {
			url = d["url"]
		}
		lines = append(lines, field("url", url, o.MaxChars)...)
		if d["status"] != nil {
			lines = append(lines, pad10+"status="+str(d["status"]))
		}
		for _, k := range []string{"query", "body"} {
			if d[k] != nil {
				lines = append(lines, field(k, d[k], o.MaxChars)...)
			}
		}
		if d["error"] != nil {
			lines = append(lines, field("← ERROR", d["error"], o.MaxChars)...)
		}
	case "db":
		if d["rows"] != nil {
			lines = append(lines, pad10+"rows="+str(d["rows"]))
		}
		lines = append(lines, field("sql", d["sql"], o.MaxChars)...)
		if d["args"] != nil {
			lines = append(lines, field("args", d["args"], o.MaxChars)...)
		}
		if d["error"] != nil {
			lines = append(lines, field("← ERROR", d["error"], o.MaxChars)...)
		}
	default:
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if d[k] != nil {
				lines = append(lines, field(k, d[k], o.MaxChars)...)
			}
		}
	}
	return lines
}

func formatLLM(d map[string]any, o FormatOptions) []string {
	var lines []string
	usage, _ := d["usage"].(map[string]any)
	params, _ := d["params"].(map[string]any)
	var stats []string
	if d["route"] != nil {
		stats = append(stats, "route="+str(d["route"]))
	}
	if usage["prompt_tokens"] != nil {
		stats = append(stats, "in="+str(usage["prompt_tokens"]))
	}
	if usage["completion_tokens"] != nil {
		stats = append(stats, "out="+str(usage["completion_tokens"]))
	}
	if params["temperature"] != nil {
		stats = append(stats, "temp="+str(params["temperature"]))
	}
	if d["status"] != nil {
		stats = append(stats, "http="+str(d["status"]))
	}
	if len(stats) > 0 {
		lines = append(lines, pad10+strings.Join(stats, " "))
	}
	if d["system"] != nil {
		lines = append(lines, field("system", d["system"], o.MaxChars)...)
	}
	msgs, _ := d["messages"].([]any)
	shown := msgs
	if !o.Verbose && len(msgs) > 4 {
		shown = msgs[len(msgs)-4:]
	}
	if len(msgs) > len(shown) {
		lines = append(lines, fmt.Sprintf("%s…%d earlier turns (--verbose for all)", pad10, len(msgs)-len(shown)))
	}
	for _, m := range shown {
		mm, _ := m.(map[string]any)
		role := str(mm["role"])
		if role == "" {
			role = "msg"
		}
		lines = append(lines, field(role, content(mm["content"]), o.MaxChars)...)
	}
	resp, _ := d["response"].(map[string]any)
	if resp["text"] != nil {
		lines = append(lines, field("← text", resp["text"], o.MaxChars)...)
	}
	if resp["error"] != nil {
		lines = append(lines, field("← ERROR", resp["error"], o.MaxChars)...)
	}
	if d["error"] != nil {
		lines = append(lines, field("← THREW", d["error"], o.MaxChars)...)
	}
	if fr := str(resp["finishReason"]); fr != "" && fr != "stop" {
		lines = append(lines, pad10+"finish="+fr)
	}
	return lines
}

// content keeps the text of a multi-part message and names the other parts.
func content(c any) string {
	switch x := c.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			pm, _ := p.(map[string]any)
			if t, ok := pm["text"].(string); ok {
				parts = append(parts, t)
			} else {
				typ := str(pm["type"])
				if typ == "" {
					typ = "part"
				}
				parts = append(parts, "<"+typ+">")
			}
		}
		return strings.Join(parts, " ")
	}
	return jsonish(c)
}

func field(label string, v any, maxChars int) []string {
	text := jsonish(v)
	if text == "" {
		return nil
	}
	text = Clip(text, maxChars)
	head := "        │ "
	if label != "" {
		head = pad(label, 8) + "│ "
	}
	rows := strings.Split(text, "\n")
	out := []string{pad10 + head + rows[0]}
	for _, r := range rows[1:] {
		out = append(out, pad10+"        │ "+r)
	}
	return out
}

// Clip cuts text to maxChars, keeping both ends (the cause of an error is often in the last
// line) and marking how much was dropped.
func Clip(text string, maxChars int) string {
	n := utf8.RuneCountInString(text)
	if maxChars <= 0 || n <= maxChars {
		return text
	}
	rs := []rune(text)
	tail := min(2000, maxChars/4)
	head := max(0, maxChars-tail)
	return fmt.Sprintf("%s…[+%d chars]…%s", string(rs[:head]), n-head-tail, string(rs[n-tail:]))
}

func jsonish(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}

func pad(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
