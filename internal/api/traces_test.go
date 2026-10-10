package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/trace"
)

func TestTraces(t *testing.T) {
	e := newEnv(t)
	ls := &logs.Store{BufDir: filepath.Join(e.dir, "buf"), DataDir: filepath.Join(e.dir, "logs"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ls.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	e.api.Logs = ls
	ctx := context.Background()
	c := e.projectClient(t, "shop", "example.com", "*.example.com")
	push(t, c, "shop/server:v1")
	if err := c.Do(ctx, "POST", Path("projects", "shop", "services", "server", "deploy"), DeployRequest{Spec: shopSpec(), Tag: "v1"}, nil); err != nil {
		t.Fatal(err)
	}

	// Records arrive through the collector exactly as a container prints them.
	col := &logs.Collector{Store: ls}
	now := time.Now().UTC()
	emit := func(i int, rec string) {
		ts := now.Add(time.Duration(i-20) * time.Second)
		col.Forward(logs.Tag("shop", "server", 1, 1), logs.Record{Time: ts, Line: fmt.Sprintf(`{"$":"trace","seq":%d,"ts":"%s",%s}`, i, ts.Format(time.RFC3339Nano), rec)})
	}
	emit(1, `"type":"http","traceId":"t1","userId":7,"source":"http:POST /orders","name":"POST /orders","data":{"dir":"in","path":"/orders","body":{"orderId":88}}`)
	emit(2, `"type":"llm","traceId":"t1","name":"model-a","durationMs":120,"costUsd":0.002,"data":{"messages":[{"role":"user","content":"hi"}],"response":{"text":"hello"}}`)
	emit(3, `"type":"http","traceId":"t1","name":"POST /orders ←","data":{"dir":"in","status":201}`)
	emit(4, `"type":"job","traceId":"t2","source":"job:invoice.send","name":"invoice.send","data":{"invoiceId":5}`)
	emit(5, `"type":"http","traceId":"t2","name":"POST api.mail.example/send","ok":false,"data":{"dir":"out","status":500}`)
	emit(6, `"type":"job","traceId":"t2","name":"invoice.send → failed","data":{"outcome":"failed"}`)
	emit(7, `"type":"note","traceId":"-","name":"server.boot"`)
	col.Forward(logs.Tag("shop", "server", 1, 1), logs.Record{Time: now, Line: "an ordinary log line"})
	ls.FlushAll()

	// The log stream keeps only the ordinary line.
	var logLines []LogLine
	c.Logs(ctx, Path("projects", "shop", "services", "server", "logs"), func(l LogLine) { logLines = append(logLines, l) })
	if len(logLines) != 1 || logLines[0].Line != "an ordinary log line" {
		t.Fatalf("log stream: %+v", logLines)
	}

	var ss []trace.Summary
	if err := c.Do(ctx, "GET", Path("projects", "shop", "traces"), nil, &ss); err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].TraceID != "t2" || ss[1].TraceID != "t1" {
		t.Fatalf("list (orphans out, newest first): %+v", ss)
	}
	t1 := ss[1]
	if t1.Input != "POST /orders" || t1.Output != "201" || t1.LLMCalls != 1 || t1.CostUSD != 0.002 || t1.UserID != "7" || t1.Service != "server" {
		t.Fatalf("summary: %+v", t1)
	}
	if ss[0].Errors != 1 || ss[0].Output != "failed" {
		t.Fatalf("job summary: %+v", ss[0])
	}

	list := func(q string) []string {
		var ss []trace.Summary
		if err := c.Do(ctx, "GET", Path("projects", "shop", "traces")+"?"+q, nil, &ss); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, s := range ss {
			ids = append(ids, s.TraceID)
		}
		return ids
	}
	// A query matches a trace through any of its records, and the whole trace is summarised.
	if got := list(`q=%22orderId%22:88`); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("q: %v", got)
	}
	if got := list("errors=1"); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("errors: %v", got)
	}
	if got := list("user=7"); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("user: %v", got)
	}
	if got := list("source=job:"); len(got) != 1 || got[0] != "t2" {
		t.Fatalf("source: %v", got)
	}

	var d TraceDetail
	if err := c.Do(ctx, "GET", Path("projects", "shop", "traces", "t1"), nil, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Records) != 3 || d.Records[0].Seq != 1 || d.Summary.Records != 3 {
		t.Fatalf("detail: %+v", d)
	}
	if err := c.Do(ctx, "GET", Path("projects", "shop", "traces", "nope"), nil, nil); err == nil {
		t.Fatal("unknown trace found")
	}

	records := func(q string) []trace.Record {
		body, err := c.Raw(ctx, "GET", Path("projects", "shop", "trace-records")+"?"+q)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		var out []trace.Record
		dec := json.NewDecoder(body)
		for {
			var r trace.Record
			if dec.Decode(&r) != nil {
				return out
			}
			out = append(out, r)
		}
	}
	if got := records("type=llm"); len(got) != 1 || got[0].Name != "model-a" {
		t.Fatalf("records type: %+v", got)
	}
	if got := records("failures=1"); len(got) != 1 || got[0].TraceID != "t2" {
		t.Fatalf("records failures: %+v", got)
	}
	if got := records("tail=2"); len(got) != 2 || got[1].Name != "server.boot" {
		t.Fatalf("records tail: %+v", got)
	}

	// Another project cannot read these traces.
	other := e.projectClient(t, "blog", "example.org")
	if err := other.Do(ctx, "GET", Path("projects", "shop", "traces"), nil, nil); err == nil {
		t.Fatal("cross-project traces")
	}
}
