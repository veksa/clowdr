package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/spec"
)

func TestLogsAndAccess(t *testing.T) {
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

	now := time.Now()
	app := logs.Key{Project: "shop", Kind: logs.KindApp, Name: "server"}
	for i := 0; i < 5; i++ {
		ls.Append(app, logs.Line{T: now.Add(time.Duration(i-10) * time.Minute).UnixNano(), R: 1, I: 1, L: fmt.Sprint("line ", i)})
	}
	ls.Append(app, logs.Line{T: now.UnixNano(), R: 2, I: 1, E: true, L: "ERROR boom"})
	ls.Flush(app)
	acc := logs.Key{Project: "shop", Kind: logs.KindAccess, Name: "_"}
	for i, s := range []int{200, 200, 404, 502, 200} {
		ls.Append(acc, logs.Line{T: now.UnixNano(), L: fmt.Sprintf(`{"time":"%s","ip":"1.2.3.%d","host":"api.example.com","method":"GET","uri":"/p%d?x=1","status":%d,"bytes":10,"rt":0.01,"country":"DE"}`, now.Format(time.RFC3339), i%2, i%2, s)})
	}

	get := func(path string) []LogLine {
		var out []LogLine
		if err := c.Logs(ctx, path, func(l LogLine) { out = append(out, l) }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := Path("projects", "shop", "services", "server", "logs")
	if got := get(base + "?tail=2"); len(got) != 2 || got[1].Line != "ERROR boom" || !got[1].Stderr || got[1].Container != "shop-server-r2-1" {
		t.Fatalf("tail: %+v", got)
	}
	if got := get(base + `?grep=ERROR\|line%203`); len(got) != 2 {
		t.Fatalf("grep: %+v", got)
	}
	if got := get(base + "?release=r1&since=450s"); len(got) != 2 {
		t.Fatalf("release+since: %+v", got)
	}

	var top []TopEntry
	if err := c.Do(ctx, "GET", Path("projects", "shop", "access")+"?top=statuses", nil, &top); err != nil {
		t.Fatal(err)
	}
	if top[0].Key != "200" || top[0].Count != 3 {
		t.Fatalf("top: %+v", top)
	}
	c.Do(ctx, "GET", Path("projects", "shop", "access")+"?top=paths", nil, &top)
	if top[0].Key != "/p0" || top[0].Count != 3 {
		t.Fatalf("top paths: %+v", top)
	}
	var combined string
	c.Do(ctx, "GET", Path("projects", "shop", "access")+"?export=combined&status=5xx", nil, &combined)
	if !strings.Contains(combined, `"GET /p1?x=1 HTTP/1.1" 502 10`) || strings.Count(combined, "\n") != 1 {
		t.Fatalf("combined: %q", combined)
	}
	// Another project cannot read these logs.
	other := e.projectClient(t, "blog", "example.org")
	if err := other.Do(ctx, "GET", Path("projects", "shop", "access")+"?top=ips", nil, nil); err == nil {
		t.Fatal("cross-project access log")
	}
}

func shopSpec() spec.Service {
	s := svcSpec()
	s.Domains = []string{"api.example.com"}
	return s
}
