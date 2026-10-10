package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/metric"
)

func TestMetrics(t *testing.T) {
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

	col := &logs.Collector{Store: ls}
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	emit := func(replica int, minute int, body string) {
		ts := hour.Add(time.Duration(minute) * time.Minute)
		col.Forward(logs.Tag("shop", "server", 1, replica), logs.Record{Time: ts, Line: fmt.Sprintf(`{"$":"metric","ts":"%s",%s}`, ts.Format(time.RFC3339), body)})
	}
	emit(1, 1, `"name":"orders","kind":"counter","value":3,"labels":{"outcome":"paid"}`)
	emit(2, 1, `"name":"orders","kind":"counter","value":2,"labels":{"outcome":"paid"}`)
	emit(1, 2, `"name":"orders","kind":"counter","value":1,"labels":{"outcome":"failed"}`)
	emit(1, 2, `"name":"queue","kind":"gauge","value":7`)
	col.Forward(logs.Tag("shop", "server", 1, 1), logs.Record{Time: hour, Line: "a plain log line"})
	ls.FlushAll()

	var infos []metric.Info
	if err := c.Do(ctx, "GET", Path("projects", "shop", "metrics")+"?since=3h", nil, &infos); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Name != "orders" || infos[0].Series != 3 || infos[1].Last != 7 {
		t.Fatalf("list: %+v", infos)
	}

	var res MetricResult
	if err := c.Do(ctx, "GET", Path("projects", "shop", "metrics", "orders")+"?since=3h&step=1h&by=outcome", nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Kind != "counter" || len(res.Series) != 2 || res.Series[1].Labels["outcome"] != "paid" || res.Series[1].Points[0].V != 5 {
		t.Fatalf("query: %+v", res)
	}
	if err := c.Do(ctx, "GET", Path("projects", "shop", "metrics", "orders")+"?since=3h&where=outcome=failed", nil, &res); err != nil || res.Series[0].Points[0].V != 1 {
		t.Fatalf("where: %+v %v", res, err)
	}
	if err := c.Do(ctx, "GET", Path("projects", "shop", "metrics", "nope")+"?since=3h", nil, &res); err == nil {
		t.Fatal("unknown metric found")
	}
	if err := c.Do(ctx, "GET", Path("projects", "shop", "metrics", "orders")+"?step=10s", nil, &res); err == nil {
		t.Fatal("step under a minute accepted")
	}

	// The log stream keeps only the plain line.
	var lines []LogLine
	c.Logs(ctx, Path("projects", "shop", "services", "server", "logs")+"?since=3h", func(l LogLine) { lines = append(lines, l) })
	if len(lines) != 1 {
		t.Fatalf("log stream: %+v", lines)
	}
}
