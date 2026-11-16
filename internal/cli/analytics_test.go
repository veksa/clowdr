package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/logs"
)

func TestAnalyticsCommands(t *testing.T) {
	fixture, err := filepath.Abs("../analytics/testdata/records.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.ok("project", "create", "shop", "--domains", "shop.example.com")

	// The offline check needs no server.
	out, _, err := h.run("analytics", "check", fixture)
	if err == nil {
		t.Fatal("invalid records in the fixture: exit code 1")
	}
	contains(t, out, "30 lines: 30 records (2 invalid", "installs 2, reinstalls 2, web first visits 1, suspicious 1", "episode_viewed")
	good := filepath.Join(h.dir, "good.ndjson")
	os.WriteFile(good, []byte(`{"$":"analytics","v":1,"id":"1","ts":"2026-11-10T10:00:00Z","e":"$pageview","d":"d1","app":"mobile","pf":"ios"}`+"\n"), 0o644)
	contains(t, h.ok("analytics", "check", good), "1 records (0 invalid")

	// Feed the fixture as received now-ish, shifted so its days are the last three.
	f, _ := os.Open(fixture)
	defer f.Close()
	k := logs.Key{Project: "shop", Kind: logs.KindAnalytics, Name: "server"}
	sc := bufio.NewScanner(f)
	day0 := time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n-3).Format("2006-01-02") }
	for sc.Scan() {
		line := sc.Text()
		// The latest first: a shifted day must not be shifted again.
		for i := 2; i >= 0; i-- {
			line = strings.ReplaceAll(line, fmt.Sprintf("2026-11-1%d", i), day(i))
		}
		l := logs.Line{T: time.Now().Add(-time.Minute).UnixNano(), L: line}
		h.srv.Logs.Append(k, l)
		h.srv.Analytics.OnLine(k, l)
	}
	p, _ := h.srv.Analytics.Project("shop")
	p.Sync()

	contains(t, h.ok("analytics", "trends", "episode_viewed", "--since", day0), "TIME", "episode_viewed", "TOTAL", "3")
	out, errOut, err := h.run("analytics", "trends", "episode_viewed", "--since", day0, "--breakdown", "episode_number", "--where", "pf=android")
	if err != nil {
		t.Fatal(err, errOut)
	}
	_, _, err = h.run("analytics", "trends", "episode_viewed", "--since", day0, "--by", "pf")
	if err != nil {
		t.Fatal(err, errOut)
	}
	contains(t, out, "TIME")
	contains(t, errOut, "1 test persons left out")
	contains(t, h.ok("analytics", "funnel", "$first_open", "registration_started", "--since", day0), "STEP", "$first_open", "20.0%")
	contains(t, h.ok("analytics", "retention", "--period", "day", "--since", day0), "COHORT", "SIZE", "+1")
	contains(t, h.ok("analytics", "stickiness", "--since", day0), "DAYS", "ACTORS")
	contains(t, h.ok("analytics", "lifecycle", "--event", "any", "--interval", "day", "--since", day0), "NEW", "DORMANT")
	out = h.ok("analytics", "paths", "--since", day0)
	contains(t, out, "PATH", "main → drama", "SCREEN", "VIEWS")
	if strings.Contains(out, "-  -") || strings.Contains(out, "-\t-") {
		t.Fatalf("screen rows printed under the path columns:\n%s", out)
	}
	contains(t, h.fails("analytics", "paths", "--since", day0, "--depth", "2"), "3 to 5")
	contains(t, h.ok("analytics", "paths", "--since", day0, "--limit", "1", "--depth", "4"), "PATH")
	contains(t, h.ok("analytics", "trends", "episode_viewed", "--since", day0, "--where", "pf=android", "--where", "pf=ios"), "TOTAL")
	contains(t, h.ok("analytics", "trends", "episode_viewed", "--since", day0, "--where", "p.drama_id>=1", "--by", "pf", "--by", "episode_number"), "TOTAL")
	contains(t, h.fails("analytics", "trends", "x_y", "--by", "a", "--by", "b", "--by", "c"), "at most two")
	contains(t, h.fails("analytics", "trends", "x_y", "--where", "p.n>abc"), "compares numbers")
	contains(t, h.ok("analytics", "person", "870", "--events", "5"), "user 870", "role = viewer", "source organic")
	contains(t, h.ok("analytics", "events", "--event", "episode_viewed", "--tail", "5", "--since", day0), `"e":"episode_viewed"`)
	contains(t, h.ok("analytics", "installs", "--since", day0), "SOURCE", "organic", "apple_search_ads")
	contains(t, h.ok("analytics", "catalog"), "EVENT", "episode_viewed")
	contains(t, h.ok("analytics", "-o", "json", "catalog"), `"kind": "catalog"`)
	contains(t, h.fails("analytics", "trends", "x_y", "--where", "nope"), "expected k=v")
	contains(t, h.fails("analytics", "trends", "x_y", "--math", "median"), "math")

	// Secrets from stdin, status.
	root := func(stdin string, args ...string) (string, error) {
		var out, errb strings.Builder
		r := newRoot(&app{version: "test", out: &out, err: &errb})
		r.SetArgs(args)
		r.SetIn(strings.NewReader(stdin))
		r.SetOut(&out)
		r.SetErr(&errb)
		err := r.Execute()
		return errb.String(), err
	}
	if msg, err := root("-100\n", "analytics", "secret", "set", "telegram-chat"); err != nil || !strings.Contains(msg, "set: telegram-chat") {
		t.Fatal(msg, err)
	}
	if _, err := root("x", "analytics", "secret", "set", "password"); err == nil {
		t.Fatal("unknown secret")
	}
	contains(t, h.ok("analytics", "status"), "configured", "secrets", "telegram-chat", "services", "server")

	// The digest: printed only with --dry-run; sending needs the configuration.
	contains(t, h.ok("analytics", "digest", "--dry-run", "--day", day(0)), "DAU")
	h.fails("analytics", "digest", "--day", day(0))
	h.fails("analytics", "archive")
	h.fails("analytics", "rebuild")
	errOut = h.ok("analytics", "rebuild", "--since", time.Now().UTC().Format("2006-01-02"))
	contains(t, errOut, "rebuilt")

	// apply sends the analytics block, diff shows the difference.
	h.ok("apply")
	os.WriteFile(filepath.Join(h.dir, "deploy.yml"), []byte(shopDeploy+"analytics:\n  installs: hourly\n  digest: { hour: 8, name: Shop }\n"), 0o644)
	out, _, err = h.run("diff")
	if err == nil {
		t.Fatal("diff: analytics differs, exit 1")
	}
	contains(t, out, "~ analytics: the config changes")
	errOut = h.ok("apply")
	contains(t, errOut, "analytics config applied")
	out, _, _ = h.run("diff") // exit 1 anyway: web is not deployed
	if strings.Contains(out, "analytics") {
		t.Fatal(out)
	}
	contains(t, h.ok("analytics", "status"), "configured  true")
	os.WriteFile(filepath.Join(h.dir, "deploy.yml"), []byte(shopDeploy), 0o644)
	errOut = h.ok("apply")
	contains(t, errOut, "analytics config applied")
	if p.Config() != nil {
		t.Fatal("removed from deploy.yml, still on the server")
	}
}
