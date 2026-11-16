package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/veksa/clowdr/internal/config"
	"html"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/metric"
)

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// minShareBase is the smallest base a share is printed for; below it only the numbers are.
const minShareBase = 20

// SendDigest builds the digest of a day and sends it to the project's Telegram chat.
func (p *Project) SendDigest(ctx context.Context, day string) error {
	tg := p.telegram()
	if tg == nil {
		return errors.New("telegram-token and telegram-chat are not set (clowctl analytics secret set)")
	}
	text, err := p.Digest(day)
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	return tg.Deliver(ctx, text)
}

// Digest is the text of a day's digest (Telegram HTML); "" when there is nothing to say.
func (p *Project) Digest(day string) (string, error) {
	cfg := p.Config()
	if cfg == nil {
		cfg = &config.AnalyticsConf{} // a dry run before deploy.yml has the block: the defaults
	}
	if _, err := time.Parse(DayLayout, day); err != nil {
		return "", fmt.Errorf("bad day %q", day)
	}
	lang := p.lang()
	l := labels(lang)
	d := dayTime(day)
	p.ensureRollups(days(d.AddDate(0, 0, -29), d))
	var lines []string
	lines = append(lines, fmt.Sprintf("ℹ️ <b>%s %s %s %s</b> (UTC)", l["digest"], html.EscapeString(p.displayName()), l["for"], d.Format("02.01")))
	body := p.eventLines(day, l)
	if cfg.Digest != nil && cfg.Digest.Access {
		body = append(body, p.accessLines(d, l)...)
	}
	if len(body) == 0 {
		body = append(body, l["no_data"])
	}
	lines = append(lines, body...)
	if w := p.warnings(day, l); w != "" {
		lines = append(lines, "⚠️ "+w)
	}
	return strings.Join(lines, "\n"), nil
}

func code(s string) string { return "<code>" + s + "</code>" }

// share prints n of base as a percentage, or nothing when the base is small.
func share(n, base int) string {
	if base < minShareBase || base == 0 {
		return ""
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(base))
}

func (p *Project) eventLines(day string, l map[string]string) []string {
	r := p.day(day)
	if r == nil {
		return nil
	}
	d := dayTime(day)
	q := &qctx{p: p, now: d.Add(48 * time.Hour)}
	var out []string

	dau := r.Active.Card()
	if dau > 0 {
		avg, n := 0, 0
		for i := 1; i <= 7; i++ {
			if pr := p.day(d.AddDate(0, 0, -i).Format(DayLayout)); pr != nil {
				avg += pr.Active.Card()
				n++
			}
		}
		line := fmt.Sprintf("%s: %s", l["active"], code("DAU "+thousands(dau)))
		if n > 0 {
			mean := float64(avg) / float64(n)
			delta := ""
			if mean >= minShareBase {
				delta = fmt.Sprintf(", %+.0f%%", 100*(float64(dau)-mean)/mean)
			}
			line += fmt.Sprintf(" (%s %s%s)", l["avg7"], thousands(int(mean+0.5)), delta)
		}
		wau := q.periodSet("any", d.AddDate(0, 0, -6), d).Card()
		mau := q.periodSet("any", d.AddDate(0, 0, -29), d).Card()
		line += fmt.Sprintf(" · %s · %s", code("WAU "+thousands(wau)), code("MAU "+thousands(mau)))
		out = append(out, line)
	}

	in := r.Installs
	var news []string
	if n := in.Get(KindInstall).N; n > 0 {
		news = append(news, l["installs"]+" "+code(thousands(n)))
	}
	if n := in.Get(KindReinstall).N; n > 0 {
		news = append(news, l["reinstalls"]+" "+code(thousands(n)))
	}
	if n := in.Get(KindFirstVisit).N; n > 0 {
		news = append(news, l["first_visit"]+" "+code(thousands(n)))
	}
	if e := r.Events["user_signed_up"]; e != nil && e.N > 0 {
		news = append(news, l["signups"]+" "+code(thousands(e.N)))
	}
	if len(news) > 0 {
		out = append(out, l["new"]+": "+strings.Join(news, " · "))
	}

	if in != nil {
		pf, src, cc := counter{}, counter{}, counter{}
		for _, k := range []string{KindInstall, KindReinstall} {
			agg := in.Get(k)
			for v, c := range agg.By["pf"] {
				pf.addN(v, c.N)
			}
			for v, c := range agg.By["source"] {
				src.addN(shortSource(v), c.N)
			}
			for v, c := range agg.By["cc"] {
				cc.addN(v, c.N)
			}
		}
		var parts []string
		for _, c := range []counter{pf, src, cc} {
			if s := c.top(3, l["more"]); s != "" {
				parts = append(parts, code(s))
			}
		}
		if len(parts) > 0 {
			out = append(out, l["installs_line"]+": "+strings.Join(parts, " | "))
		}
	}

	var ret []string
	for _, off := range []int{1, 7} {
		cd := d.AddDate(0, 0, -off)
		cr := p.day(cd.Format(DayLayout))
		if cr == nil || cr.First.Card() == 0 {
			continue
		}
		base := cr.First.Card()
		back := and(cr.First, r.Active).Card()
		val := share(back, base)
		if val == "" {
			val = fmt.Sprintf("%d", back)
		}
		ret = append(ret, fmt.Sprintf("D%d %s %s: %s (%s %d)", off, l["cohort"], cd.Format("02.01"), code(val), l["of"], base))
	}
	if len(ret) > 0 {
		out = append(out, l["retention"]+" "+strings.Join(ret, " · "))
	}

	n := func(e string) int {
		if a := r.Events[e]; a != nil {
			return a.N
		}
		return 0
	}
	if buys, st, ren, can, ref := n("purchase_completed"), n("subscription_started"), n("subscription_renewed"), n("subscription_canceled"), n("subscription_refunded")+n("purchase_refunded"); buys+st+ren+can+ref > 0 {
		out = append(out, fmt.Sprintf("%s: %s %s · %s +%s / %s %s / %s %s / %s %s", l["money"], l["purchases"], code(thousands(buys)),
			l["subs"], code(thousands(st)), l["renewed"], code(thousands(ren)), l["canceled"], code(thousands(can)), l["refunds"], code(thousands(ref))))
	}

	if cfg := p.Config(); cfg != nil && cfg.Digest != nil && len(cfg.Digest.KeyEvents) > 0 {
		var parts []string
		for _, e := range cfg.Digest.KeyEvents {
			if a := r.Events[e]; a != nil && a.N > 0 {
				parts = append(parts, fmt.Sprintf("%s %s (%s %s)", html.EscapeString(e), code(thousands(a.N)), thousands(a.U.Card()), l["people"]))
			}
		}
		if len(parts) > 0 {
			out = append(out, l["key"]+": "+strings.Join(parts, " · "))
		}
	}

	if s := r.Sessions; s != nil && s.N > 0 {
		line := fmt.Sprintf("%s: %s", l["sessions"], code(thousands(s.N)))
		if s.Ended > 0 {
			line += fmt.Sprintf(", %s %s", l["median"], code(fmt.Sprintf("%.0f %s", float64(s.DurP50)/60000, l["min"])))
		}
		var cat Catalog
		p.state.GetMeta("catalog", &cat)
		if cat.Events[EventClientError] != nil && dau > 0 {
			crashed := 0
			if e := r.Events[EventClientError]; e != nil {
				if c := e.By["fatal"]["1"]; c != nil {
					crashed = and(c.U, r.Active).Card()
				}
				if c := e.By["fatal"]["true"]; c != nil {
					crashed += and(c.U, r.Active).Card()
				}
			}
			if dau >= minShareBase {
				line += fmt.Sprintf(" · %s %s %s", l["crash_free"], code(fmt.Sprintf("%.1f%%", 100*float64(dau-crashed)/float64(dau))), l["users"])
			}
		}
		out = append(out, line)
	}

	if av := r.ActiveBy["av"]; len(av) > 0 && dau >= minShareBase {
		vals := map[string]int{}
		total := 0
		for v, c := range av {
			vals[v] = c.U.Card()
			total += c.U.Card()
		}
		top := topKeys(vals, 2)
		var parts []string
		rest := total
		for _, v := range top {
			parts = append(parts, fmt.Sprintf("%s %s", html.EscapeString(v), share(vals[v], total)))
			rest -= vals[v]
		}
		if rest > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", l["older"], share(rest, total)))
		}
		out = append(out, l["versions"]+": "+code(strings.Join(parts, " · ")))
	}
	return out
}

// warnings is the ⚠️ line: archive days not uploaded for two days, and what the server dropped.
func (p *Project) warnings(day string, l map[string]string) string {
	var parts []string
	if cfg := p.Config(); cfg != nil && cfg.ArchiveOn() {
		a := p.archiveState()
		ws := p.WindowStart().Format(DayLayout)
		d := dayTime(day)
		var missing []string
		for i := 7; i >= 1; i-- {
			md := d.AddDate(0, 0, -i).Format(DayLayout)
			if md < ws {
				continue
			}
			if _, ok := a.Raw[md]; !ok {
				missing = append(missing, dayTime(md).Format("02.01"))
			}
		}
		switch len(missing) {
		case 0:
		case 1:
			parts = append(parts, fmt.Sprintf(l["archive_missing"], missing[0]))
		default:
			parts = append(parts, fmt.Sprintf(l["archive_missing_n"], len(missing), missing[0]))
		}
	}
	if dropped := p.Dropped(dayTime(day), dayTime(day).AddDate(0, 0, 1)); len(dropped) > 0 {
		keys := make([]string, 0, len(dropped))
		for k := range dropped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var ds []string
		for _, k := range keys {
			ds = append(ds, fmt.Sprintf("%s %d", html.EscapeString(k), int(dropped[k])))
		}
		parts = append(parts, l["dropped"]+": "+strings.Join(ds, ", "))
	}
	return strings.Join(parts, " · ")
}

// DroppedMetric is the counter the server-side package writes for events it refused.
const DroppedMetric = "analytics_dropped_total"

// Dropped sums the server's dropped counters by reason over [from, to).
func (p *Project) Dropped(from, to time.Time) map[string]float64 {
	out := map[string]float64{}
	if p.m.Logs == nil {
		return out
	}
	for _, svc := range p.m.Logs.Streams(p.name, logs.KindMetric) {
		k := logs.Key{Project: p.name, Kind: logs.KindMetric, Name: svc}
		p.m.Logs.Read(k, logs.Query{Since: from, Until: to, Match: func(l logs.Line) bool { return strings.Contains(l.L, DroppedMetric) }}, func(l logs.Line) {
			pt, ok := metric.Parse(l.L)
			if !ok || pt.Name != DroppedMetric || pt.Kind != metric.Counter {
				return
			}
			if t := pt.Time(); t.Before(from) || !t.Before(to) {
				return
			}
			reason := pt.Labels["reason"]
			if reason == "" {
				reason = "unknown"
			}
			out[reason] += pt.Value
		})
	}
	for k, v := range out {
		if v == 0 {
			delete(out, k)
		}
	}
	return out
}

// ─── Access log digest (sites without events) ────────────────────────────────

var (
	botUA      = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|curl|wget|python|go-http|headless|preview|monitor|scan|fetch|http-client|java/|okhttp|axios|node-fetch|lighthouse|pingdom|uptime`)
	staticExts = map[string]bool{".js": true, ".css": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
		".ico": true, ".webp": true, ".avif": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".map": true,
		".json": true, ".xml": true, ".txt": true, ".webmanifest": true, ".mp4": true, ".webm": true, ".mp3": true, ".pdf": true, ".zip": true, ".wasm": true}
)

type accessEntry struct {
	Host   string `json:"host"`
	Method string `json:"method"`
	URI    string `json:"uri"`
	Status int    `json:"status"`
	Ref    string `json:"ref"`
	UA     string `json:"ua"`
	IP     string `json:"ip"`
}

// AccessSummary is a day of a site from the access log: page views of people, roughly how many
// people (distinct address and browser, counted in memory and not kept), top pages and referrers.
type AccessSummary struct {
	Views     int
	Visitors  int
	Pages     counter
	Referrers counter
}

// AccessDay summarises the access log of [from, from+24h).
func (p *Project) AccessDay(from time.Time) AccessSummary {
	s := AccessSummary{Pages: counter{}, Referrers: counter{}}
	if p.m.Logs == nil {
		return s
	}
	to := from.Add(24 * time.Hour)
	visitors := map[string]bool{}
	hosts := map[string]bool{}
	p.m.Logs.Read(logs.Key{Project: p.name, Kind: logs.KindAccess, Name: "_"}, logs.Query{Since: from, Until: to}, func(l logs.Line) {
		if l.Time().Before(from) || !l.Time().Before(to) {
			return
		}
		var e accessEntry
		if json.Unmarshal([]byte(l.L), &e) != nil {
			return
		}
		// A redirect is not a view: the page it leads to is logged on its own (a trailing slash, an
		// alias domain). 304 is: the browser revalidated a page it shows.
		if e.Method != "GET" || (e.Status < 200 || e.Status >= 300) && e.Status != 304 || botUA.MatchString(e.UA) || e.UA == "" {
			return
		}
		pth, _, _ := strings.Cut(e.URI, "?")
		if ext := strings.ToLower(path.Ext(pth)); ext != "" && ext != ".html" && ext != ".htm" || staticExts[strings.ToLower(path.Ext(pth))] {
			return
		}
		if strings.HasPrefix(pth, "/_next/") || strings.HasPrefix(pth, "/api/") {
			return
		}
		hosts[strings.ToLower(e.Host)] = true
		s.Views++
		visitors[e.IP+"\x00"+e.UA] = true
		s.Pages.add(pth)
		if u, err := url.Parse(e.Ref); err == nil && u.Host != "" {
			s.Referrers.add(strings.TrimPrefix(strings.ToLower(u.Host), "www."))
		}
	})
	for h := range hosts { // a click inside the site is not a referrer
		delete(s.Referrers, strings.TrimPrefix(h, "www."))
	}
	s.Visitors = len(visitors)
	return s
}

func (p *Project) accessLines(day time.Time, l map[string]string) []string {
	s := p.AccessDay(day)
	if s.Views == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("%s: %s · %s ≈%s", l["views"], code(thousands(s.Views)), l["visitors"], code(thousands(s.Visitors)))}
	if t := s.Pages.top(5, l["more"]); t != "" {
		out = append(out, l["pages"]+": "+code(t))
	}
	if t := s.Referrers.top(5, l["more"]); t != "" {
		out = append(out, l["referrers"]+": "+code(t))
	}
	return out
}
