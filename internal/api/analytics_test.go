package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/analytics"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/logs"
)

func TestAnalyticsAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.projectClient(t, "shop", "example.com")
	path := func(p ...string) string { return Path(append([]string{"projects", "shop", "analytics"}, p...)...) }

	// Without the manager every route answers 501 and the feature is not listed.
	var v Version
	e.admin.Do(ctx, "GET", "/v1/version", nil, &v)
	if slices.Contains(v.Features, FeatureAnalytics) {
		t.Fatal("feature listed without analytics")
	}
	if err := c.Do(ctx, "GET", path("status"), nil, nil); err == nil || !strings.Contains(err.Error(), "no analytics") {
		t.Fatalf("501: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ls := &logs.Store{BufDir: filepath.Join(e.dir, "buf"), DataDir: filepath.Join(e.dir, "logs"), Log: log}
	if err := ls.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	m := &analytics.Manager{Dir: filepath.Join(e.dir, "analytics"), Logs: ls, Log: log}
	t.Cleanup(m.Close)
	e.api.Logs, e.api.Analytics = ls, m
	e.admin.Do(ctx, "GET", "/v1/version", nil, &v)
	if !slices.Contains(v.Features, FeatureAnalytics) {
		t.Fatal(v.Features)
	}

	col := &logs.Collector{Store: ls, OnLine: m.OnLine}
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1).Format(analytics.DayLayout)
	for i, l := range []string{
		`{"$":"analytics","v":1,"id":"a1","ts":"` + yesterday + `T10:00:00Z","e":"$first_open","d":"dev1","app":"mobile","pf":"ios","ik":"k1"}`,
		`{"$":"analytics","v":1,"id":"a2","ts":"` + yesterday + `T10:01:00Z","e":"episode_viewed","u":"7","d":"dev1","app":"mobile","pf":"ios","dm":{"episode_number":"1"}}`,
		`{"$":"analytics","v":1,"id":"a3","ts":"` + yesterday + `T10:02:00Z","e":"episode_viewed","u":"8","d":"dev2","app":"mobile","pf":"android"}`,
	} {
		col.Forward(logs.Tag("shop", "server", 1, 1), logs.Record{Time: now.Add(time.Duration(i) * time.Second), Line: l})
	}
	ls.FlushAll()
	p, _ := m.Project("shop")
	p.Sync()

	// The config: stored, read back, refused when invalid, removed with null.
	hour := 9
	cfg := config.AnalyticsConf{Installs: config.InstallsHourly, Digest: &config.DigestConf{Hour: &hour, Name: "Shop"}}
	var ch map[string]bool
	if err := c.Do(ctx, "PUT", path("config"), cfg, &ch); err != nil || !ch["changed"] {
		t.Fatal(ch, err)
	}
	if err := c.Do(ctx, "PUT", path("config"), cfg, &ch); err != nil || ch["changed"] {
		t.Fatal("unchanged config counted as changed", ch, err)
	}
	var back *config.AnalyticsConf
	if err := c.Do(ctx, "GET", path("config"), nil, &back); err != nil || back == nil || back.InstallsMode() != config.InstallsHourly {
		t.Fatal(back, err)
	}
	if err := c.Do(ctx, "PUT", path("config"), config.AnalyticsConf{Installs: "often"}, nil); err == nil {
		t.Fatal("invalid config accepted")
	}

	var sec map[string][]string
	if err := c.Do(ctx, "PUT", path("secrets", "telegram-chat"), AnalyticsSecretRequest{Value: "-100"}, &sec); err != nil || len(sec["secrets"]) != 1 {
		t.Fatal(sec, err)
	}
	if err := c.Do(ctx, "PUT", path("secrets", "password"), AnalyticsSecretRequest{Value: "x"}, nil); err == nil {
		t.Fatal("unknown secret accepted")
	}

	var st analytics.Status
	if err := c.Do(ctx, "GET", path("status"), nil, &st); err != nil || !st.Configured || st.Last24h != 3 || st.Services[0] != "server" {
		t.Fatalf("status %+v %v", st, err)
	}

	var res analytics.Result
	spec := analytics.Spec{Kind: "trends", Events: []string{"episode_viewed"}, Since: yesterday, Until: yesterday + "T23:59:59Z"}
	if err := c.Do(ctx, "POST", path("query"), spec, &res); err != nil || res.Series[0].Total != 2 {
		t.Fatalf("query %+v %v", res, err)
	}
	if err := c.Do(ctx, "POST", path("query"), analytics.Spec{Kind: "nope"}, nil); err == nil {
		t.Fatal("bad query accepted")
	}

	var dg AnalyticsDigest
	if err := c.Do(ctx, "POST", path("digest")+"?dry_run=1", nil, &dg); err != nil || dg.Sent || dg.Day != yesterday || !strings.Contains(dg.Text, "DAU <code>") && !strings.Contains(dg.Text, "DAU 2") {
		t.Fatalf("digest %+v %v", dg, err)
	}
	// Sending needs the bot token too.
	if err := c.Do(ctx, "POST", path("digest"), nil, nil); err == nil {
		t.Fatal("digest sent without a bot token")
	}

	// No archive configured: an error, not an empty list.
	if err := c.Do(ctx, "GET", path("archive")+"?since=3d", nil, nil); err == nil {
		t.Fatal("archive URLs without an archive")
	}

	// Rebuild is the admin's.
	if _, err := c.Stream(ctx, "POST", path("rebuild")+"?since=3d", nil, func(Event) {}); err == nil {
		t.Fatal("a project token rebuilt")
	}
	var msgs []string
	// From today: the stream began today, and before it there is no archive to read.
	if done, _ := e.admin.Stream(ctx, "POST", path("rebuild")+"?since=3d", nil, func(Event) {}); done == nil || !strings.Contains(done.Error, "no archive configured") {
		t.Fatalf("rebuild before the stream without an archive: %+v", done)
	}
	done, err := e.admin.Stream(ctx, "POST", path("rebuild")+"?since="+now.Format(analytics.DayLayout), nil, func(ev Event) { msgs = append(msgs, ev.Message) })
	if err != nil || done.Error != "" || len(msgs) == 0 {
		t.Fatalf("rebuild %+v %v %v", done, msgs, err)
	}
	if err := e.admin.Do(ctx, "POST", path("rebuild"), nil, nil); err == nil {
		t.Fatal("rebuild without since")
	}

	if err := c.Do(ctx, "DELETE", path("config"), nil, &ch); err != nil || !ch["changed"] {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "GET", path("config"), nil, &back); err != nil || back != nil {
		t.Fatal("config not removed", back)
	}
	if err := e.admin.Do(ctx, "GET", Path("projects", "ghost", "analytics", "status"), nil, nil); err == nil {
		t.Fatal("a missing project")
	}
}
