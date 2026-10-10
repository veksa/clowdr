package trace

import (
	"strings"
	"testing"
)

func rec(line string) Record {
	r, ok := Parse(line)
	if !ok {
		panic(line)
	}
	return r
}

func TestFormatTrace(t *testing.T) {
	rs := []Record{
		rec(`{"$":"trace","seq":2,"ts":"2026-01-02T10:00:00.120Z","type":"llm","traceId":"t1","name":"model-a","durationMs":120,"costUsd":0.0041,"data":{"usage":{"prompt_tokens":10,"completion_tokens":3},"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url"}]}],"response":{"text":"ok","finishReason":"length"}}}`),
		rec(`{"$":"trace","seq":1,"ts":"2026-01-02T10:00:00.000Z","type":"http","traceId":"t1","userId":"u-1","source":"http:GET /a","tags":{"chat":"42","tool":"x"},"name":"GET /a","data":{"dir":"in","path":"/a"}}`),
		rec(`{"$":"trace","seq":3,"ts":"2026-01-02T10:00:00.200Z","type":"log","traceId":"t1","name":"Orders","level":"error","data":{"message":"line one\nline two"}}`),
	}
	Sort(rs)
	s := Summarize("t1", rs)
	if s.DurationMs != 200 || s.UserID != "u-1" || s.Errors != 1 || s.Input != "GET /a" || len(s.Models) != 1 {
		t.Fatalf("summary: %+v", s)
	}
	out := FormatTrace(s, rs, FormatOptions{})
	for _, want := range []string{
		"═══ TRACE t1", "who     user=u-1 source=http:GET /a", "tags    chat=42 tool=x",
		"+120ms    llm       model-a  [120ms $0.004100]", "in=10 out=3",
		"user    │ look <image_url>", "← text  │ ok", "finish=length",
		"+200ms    log       Orders  [error]", "        │ line two",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if FormatSummaries(nil) != "(no traces)" || FormatRecords(nil, FormatOptions{}) != "(no records)" {
		t.Error("empty renderings")
	}
}

func TestClipKeepsBothEnds(t *testing.T) {
	s := strings.Repeat("a", 100) + "END"
	got := Clip(s, 40)
	if !strings.HasPrefix(got, strings.Repeat("a", 30)) || !strings.HasSuffix(got, "aaaaaaaEND") || !strings.Contains(got, "…[+63 chars]…") {
		t.Fatalf("clip: %q", got)
	}
	if Clip("short", 40) != "short" {
		t.Fatal("short text clipped")
	}
}

func TestParseRejectsNonTraces(t *testing.T) {
	for _, l := range []string{"plain text", `{"$":"trace","seq":1`, `{"msg":"json log"}`} {
		if _, ok := Parse(l); ok {
			t.Errorf("parsed %q", l)
		}
	}
}
