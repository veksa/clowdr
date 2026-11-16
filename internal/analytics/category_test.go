package analytics

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
)

var testApps = &config.AppsConf{Product: []string{"mobile", "dashboard"}, Site: []string{"landing"}, Internal: []string{"admin"}}

// mixedStream is two days of a product with a site and an admin panel: 35 phones (25 iOS on two
// versions, 10 Android), one of them a signed-in buyer, 40 site visitors and one phone user on
// the site under the same device id, a moderator in the admin panel, an author on the dashboard,
// and server records with and without a person. On the second day 20 iOS and 5 Android phones
// come back, and 10 visitors.
func mixedStream() []string {
	var out []string
	n := 0
	rec := func(day int, e, actor, app, pf, av string) {
		n++
		out = append(out, fmt.Sprintf(`{"$":"analytics","v":1,"id":"m%d","ts":"2026-11-%02dT10:%02d:00Z","e":"%s",%s,"s":"s%d-%s","app":"%s","pf":"%s","av":"%s"}`,
			n, day, n%60, e, actor, day, strings.ReplaceAll(actor[5:], `"`, ""), app, pf, av))
	}
	dev := func(i int) string { return fmt.Sprintf(`"d":"p%d"`, i) }
	for day := 10; day <= 11; day++ {
		for i := 0; i < 35; i++ {
			if day == 11 && !(i < 20 || i >= 25 && i < 30) {
				continue
			}
			pf, av := "ios", "2.3.1"
			if i%5 == 0 {
				av = "2.3.0"
			}
			if i >= 25 {
				pf, av = "android", "2.3.2"
			}
			rec(day, "$pageview", dev(i), "mobile", pf, av)
			if day == 10 {
				rec(day, "$first_open", dev(i), "mobile", pf, av)
			}
		}
		for i := 0; i < 40; i++ {
			if day == 11 && i >= 10 {
				continue
			}
			rec(day, "$pageview", fmt.Sprintf(`"d":"w%d"`, i), "landing", "web", "1.0.0")
		}
		rec(day, "$pageview", `"d":"p3"`, "landing", "web", "1.0.0") // a phone user on the site
		rec(day, "$pageview", `"u":"mod","d":"a1"`, "admin", "web", "0.9.0")
		rec(day, "item_reviewed", `"u":"mod","d":"a1"`, "server", "server", "")
		rec(day, "order_paid", `"u":"buyer","d":"p1"`, "server", "server", "") // a person: product
		rec(day, "webhook_seen", `"d":"hook"`, "server", "server", "")         // no person: nowhere
		rec(day, "dashboard_opened", `"u":"author","d":"b1"`, "dashboard", "web", "1.4.0")
	}
	return out
}

func TestCategoriesDigest(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{Apps: testApps}, nil)
	e.feed(time.Time{}, mixedStream()...)
	p := e.project()
	p.Recompute([]string{"2026-11-10", "2026-11-11"})
	text, err := p.Digest("2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + text)
	// Day 11 in the product: 25 phones (the buyer's among them: the server's record links its
	// device), the author on the dashboard, and the moderator through the server's own record (a
	// record of no listed app counts by its person). The webhook without a person and the site's
	// visitors are not in it.
	for _, want := range []string{
		"Активные: <code>DAU 27</code> (iOS 20 · Android 5 · web 1)",
		"<code>WAU 37</code>",
		"Удержание D1 когорты 10.11: <code>73%</code> (из 37; iOS 80% · Android 5/10 · web 1/1)",
		"Версии: <code>iOS 2.3.1 80% · 2.3.0 20%</code>",
		"Сайт: посетители <code>11</code> (новые 0) · из них в продукте за 30 дн. <code>1</code>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "💡") {
		t.Errorf("hint with apps set:\n%s", text)
	}

	// Without analytics.apps: everything together, and the hint.
	e.configure(config.AnalyticsConf{}, nil)
	text, _ = p.Digest("2026-11-11")
	if !strings.Contains(text, "DAU 38") || !strings.Contains(text, "💡") {
		t.Errorf("without apps:\n%s", text)
	}
}

// TestCategoryQueries checks that a category and a platform count the same from rollups and
// from raw records, and that a changed categorisation is recomputed in the hot window.
func TestCategoryQueries(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{Apps: testApps}, nil)
	e.feed(time.Time{}, mixedStream()...)
	p := e.project()
	p.Recompute([]string{"2026-11-10", "2026-11-11"})
	ctx := context.Background()
	q := func(s Spec) *Result {
		t.Helper()
		s.Since, s.Until = "2026-11-10", "2026-11-11T23:59:59Z"
		if s.Kind == "" {
			s.Kind = "trends"
		}
		res, err := p.Query(ctx, s)
		if err != nil {
			t.Fatalf("%+v: %v", s, err)
		}
		return res
	}
	points := func(r *Result) string {
		var ps []string
		for _, pt := range r.Series[0].Points {
			ps = append(ps, fmt.Sprint(pt.V))
		}
		return strings.Join(ps, ",")
	}
	// A filter the rollups cannot answer sends the same question to the raw records.
	rawOnly := []string{"p.none!=x"}
	for _, c := range []struct {
		app, pf, want string
	}{
		{"", "", "37,27"},        // default: product
		{"product", "", "37,27"}, // 35 phones, the author, the moderator on the server; then 25 + 2
		{"site", "", "41,11"},    // 40 visitors and the phone user on the site
		{"internal", "", "1,1"},  // the moderator in the panel
		{"all", "", "78,38"},     // every actor, the webhook's device too
		{"product", "ios", "25,20"},
		{"product", "android", "10,5"},
		{"mobile", "", "35,25"}, // one app (of the product)
		{"", "web", "1,1"},      // the product on the web: the dashboard's author
	} {
		roll := q(Spec{Events: []string{"any"}, Math: "dau", App: c.app, Platform: c.pf})
		raw := q(Spec{Events: []string{"any"}, Math: "dau", App: c.app, Platform: c.pf, Filters: rawOnly})
		if roll.Source != "rollup" || raw.Source != "raw" {
			t.Fatalf("%s/%s: sources %s, %s", c.app, c.pf, roll.Source, raw.Source)
		}
		if points(roll) != points(raw) {
			t.Errorf("%s/%s: rollup %s, raw %s", c.app, c.pf, points(roll), points(raw))
		}
		if c.want != "" && points(roll) != c.want {
			t.Errorf("%s/%s: %s, want %s (covers %s)", c.app, c.pf, points(roll), c.want, roll.Covers)
		}
	}
	if r := q(Spec{Events: []string{"any"}, App: "product"}); !strings.HasPrefix(r.Covers, "product (dashboard, mobile") {
		t.Errorf("covers %q", r.Covers)
	}

	// Retention of the product: the site's visitors are not in its cohort.
	ret := q(Spec{Kind: "retention", Period: "day"})
	if c := ret.Cohorts[0]; c.Size != 37 || c.Values[1] != 27 {
		t.Errorf("product retention %+v", ret.Cohorts)
	}
	iosRet := q(Spec{Kind: "retention", Period: "day", Platform: "ios"})
	if c := iosRet.Cohorts[0]; c.Size != 25 || c.Values[1] != 20 {
		t.Errorf("iOS retention %+v", iosRet.Cohorts)
	}
	site := q(Spec{Kind: "retention", Period: "day", App: "site"})
	if c := site.Cohorts[0]; c.Size != 41 || c.Values[1] != 11 {
		t.Errorf("site retention %+v", site.Cohorts)
	}
	inst := q(Spec{Kind: "installs", Platform: "android"})
	if len(inst.Rows) != 1 || inst.Rows[0]["pf"] != "android" || inst.Rows[0]["first_visit"] != 10 {
		t.Errorf("android installs %+v", inst.Rows)
	}
	if _, err := p.Query(ctx, Spec{Kind: "retention", App: "mobile", Platform: "ios"}); err == nil {
		t.Error("an app and a platform on rollups")
	}

	// The landing moves into the product and the panel is listed nowhere: the hot window is
	// recomputed with the new key; the moderator still counts by person.
	e.configure(config.AnalyticsConf{Apps: &config.AppsConf{Product: []string{"mobile", "dashboard", "landing"}}}, nil)
	if r := q(Spec{Events: []string{"any"}, Math: "dau", App: "product"}); points(r) != "77,37" || r.Note != "" {
		t.Errorf("after the change: %s (%s)", points(r), r.Note)
	}

	// No categories: a category is refused, an app is fine.
	e.configure(config.AnalyticsConf{}, nil)
	if _, err := p.Query(ctx, Spec{Kind: "trends", Events: []string{"any"}, App: "product"}); err == nil {
		t.Error("a category without analytics.apps")
	}
	if r := q(Spec{Events: []string{"any"}, Math: "dau"}); points(r) != "78,38" {
		t.Errorf("all apps without categories: %s", points(r))
	}
}
