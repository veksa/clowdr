package analytics

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/config"
)

// maxEachPerHour caps per-install messages; the rest of the hour goes into the hourly summary.
const maxEachPerHour = 20

// telegram returns the project's sender, or nil without a bot token and chat.
func (p *Project) telegram() Sender {
	s := p.secrets()
	if s[SecretTelegramToken] == "" || s[SecretTelegramChat] == "" {
		return nil
	}
	return p.m.sender(s[SecretTelegramToken], s[SecretTelegramChat])
}

func (p *Project) displayName() string {
	if c := p.Config(); c != nil && c.Digest != nil && c.Digest.Name != "" {
		return c.Digest.Name
	}
	return p.name
}

func (p *Project) lang() string {
	if c := p.Config(); c != nil && c.Digest != nil && c.Digest.Lang != "" {
		return c.Digest.Lang
	}
	return "ru"
}

// onInstalls sends a message per install when installs: each, at most maxEachPerHour an hour.
func (p *Project) onInstalls(ds []InstallDecision) {
	if len(ds) == 0 {
		return
	}
	cfg := p.Config()
	if cfg == nil || cfg.InstallsMode() != config.InstallsEach {
		return
	}
	tg := p.telegram()
	if tg == nil {
		return
	}
	now := p.m.now()
	for _, d := range ds {
		if d.Kind == KindFirstVisit || d.Record.suspect() {
			continue
		}
		if now.Sub(d.Record.Time()) > 2*time.Hour {
			continue // a late event from an offline queue: news only when it is news
		}
		hour := now.Format("2006-01-02T15")
		p.mu.Lock()
		if p.eachHour != hour {
			p.eachHour, p.eachSent = hour, 0
		}
		send := p.eachSent < maxEachPerHour
		if send {
			p.eachSent++
		} else {
			if p.eachMissed == nil {
				p.eachMissed = map[string]int{}
			}
			p.eachMissed[hour]++
		}
		p.mu.Unlock()
		if send {
			text := installMessage(p.lang(), p.displayName(), d)
			go func() {
				if err := tg.Deliver(context.Background(), text); err != nil {
					p.m.log().Warn("analytics: install message", "project", p.name, "err", err)
				}
			}()
		}
	}
}

// missInstalls counts installs that were indexed too late for a message of their own into the
// hourly summary of the "each" mode.
func (p *Project) missInstalls(ds []InstallDecision) {
	if cfg := p.Config(); cfg == nil || cfg.InstallsMode() != config.InstallsEach {
		return
	}
	hour := p.m.now().Format("2006-01-02T15")
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range ds {
		if d.Kind == KindFirstVisit || d.Record.suspect() {
			continue
		}
		if p.eachMissed == nil {
			p.eachMissed = map[string]int{}
		}
		p.eachMissed[hour]++
	}
}

// installMessage is the per-install text.
func installMessage(lang, name string, d InstallDecision) string {
	l := labels(lang)
	r := &d.Record
	var b strings.Builder
	fmt.Fprintf(&b, "ℹ️ <b>%s</b> · %s\n", l["install"], html.EscapeString(name))
	plat := strings.TrimSpace(strings.Join(nonEmpty(r.PF, r.AV), " "))
	if r.FF != "" {
		plat += " · " + r.FF
	}
	if plat != "" {
		fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["platform"], html.EscapeString(plat))
	}
	if r.CC != "" {
		fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["country"], html.EscapeString(r.CC))
	}
	fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["source"], html.EscapeString(sourceLabel(lang, r)))
	if d.Kind == KindReinstall {
		fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["reinstall"], fmt.Sprintf(l["reinstall_n"], max(d.N, 2)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sourceLabel names an install's source for people.
func sourceLabel(lang string, r *Record) string {
	l := labels(lang)
	src := sourceOf(r)
	campaign := str(r.DM["campaign"])
	switch src {
	case "":
		return l["unknown"]
	case "apple_search_ads":
		if campaign != "" {
			return fmt.Sprintf("Apple Search Ads · %s %s", l["campaign"], campaign)
		}
		return "Apple Search Ads"
	case "organic", "sideload", "appstore", "play", "testflight", "amazon", "adb":
		return src
	}
	s := "utm:" + src
	if campaign != "" {
		s += " · " + campaign
	}
	return s
}

// shortSource is a source as a key in summaries.
func shortSource(src string) string {
	switch src {
	case "":
		return "?"
	case "apple_search_ads":
		return "ASA"
	case "organic", "sideload", "appstore", "play", "testflight", "amazon", "adb", OtherValue:
		return src
	}
	return "utm:" + src
}

// hourlyInstalls sends the summary of the last full hour: in hourly mode when there were
// installs, in each mode when some went over the per-message limit.
func (p *Project) hourlyInstalls(ctx context.Context, now time.Time) error {
	hourEnd := now.Truncate(time.Hour)
	if now.Sub(hourEnd) < time.Minute { // let the last lines of the hour arrive
		return nil
	}
	hourStart := hourEnd.Add(-time.Hour)
	key := hourStart.Format("2006-01-02T15")
	var last string
	p.state.GetMeta("installs_hour_sent", &last)
	if last >= key {
		return nil
	}
	cfg := p.Config()
	if cfg == nil {
		return nil
	}
	if cfg.InstallsMode() == config.InstallsEach {
		p.mu.Lock()
		missed := p.eachMissed[key] > 0
		for h := range p.eachMissed {
			if h <= key {
				delete(p.eachMissed, h)
			}
		}
		p.mu.Unlock()
		if !missed {
			return p.state.SetMeta("installs_hour_sent", key)
		}
	}
	text, n, err := p.hourSummary(hourStart, hourEnd)
	if err != nil {
		return err
	}
	if n == 0 {
		return p.state.SetMeta("installs_hour_sent", key)
	}
	tg := p.telegram()
	if tg == nil {
		return p.state.SetMeta("installs_hour_sent", key)
	}
	if err := tg.Deliver(ctx, text); err != nil {
		return err
	}
	return p.state.SetMeta("installs_hour_sent", key)
}

// hourSummary counts the $first_open records received in [from, to).
func (p *Project) hourSummary(from, to time.Time) (string, int, error) {
	recs, err := p.readRecords(from, to, func(r *Record) bool { return r.E == EventFirstOpen })
	if err != nil {
		return "", 0, err
	}
	seen := map[string]bool{}
	total, re, suspect := 0, 0, 0
	pf, src, cc := counter{}, counter{}, counter{}
	p.state.View(func(t *Tx) error {
		for i := range recs {
			r := &recs[i]
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if r.suspect() {
				suspect++
				continue
			}
			kind, _ := t.installKind(r)
			if kind == KindFirstVisit {
				continue
			}
			total++
			if kind == KindReinstall {
				re++
			}
			pf.add(r.PF)
			src.add(shortSource(sourceOf(r)))
			cc.add(r.CC)
		}
		return nil
	})
	if total == 0 && suspect == 0 {
		return "", 0, nil
	}
	l := labels(p.lang())
	var b strings.Builder
	fmt.Fprintf(&b, "ℹ️ <b>%s %s–%s UTC</b> · %s\n", l["installs_for"], from.Format("15:04"), to.Format("15:04"), html.EscapeString(p.displayName()))
	fmt.Fprintf(&b, "%s: <code>%d</code> (%s %d, %s %d)\n", l["total"], total, l["reinst_short"], re, l["suspect"], suspect)
	if s := pf.top(3, l["more"]); s != "" {
		fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["platforms"], s)
	}
	if s := src.top(3, l["more"]); s != "" {
		fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["sources"], s)
	}
	if s := cc.top(3, l["more"]); s != "" {
		fmt.Fprintf(&b, "%s: <code>%s</code>\n", l["countries"], s)
	}
	return strings.TrimRight(b.String(), "\n"), total + suspect, nil
}

// counter counts keys and prints the largest.
type counter map[string]int

func (c counter) add(k string) {
	if k != "" {
		c[k]++
	}
}

func (c counter) addN(k string, n int) {
	if k != "" && n > 0 {
		c[k] += n
	}
}

// top prints "a 3 · b 2 · <more> 4" with the n largest keys and how many keys are left.
func (c counter) top(n int, more string) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range c {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var parts []string
	rest := 0
	for i, e := range all {
		if i < n {
			parts = append(parts, fmt.Sprintf("%s %s", html.EscapeString(e.k), thousands(e.v)))
		} else {
			rest += e.v
		}
	}
	if rest > 0 {
		parts = append(parts, fmt.Sprintf("%s %s", more, thousands(rest)))
	}
	return strings.Join(parts, " · ")
}

// thousands writes 1204 as "1 204".
func thousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func labels(lang string) map[string]string {
	if lang == "en" {
		return labelsEN
	}
	return labelsRU
}

var labelsRU = map[string]string{
	"install": "Установка", "platform": "Платформа", "country": "Страна", "source": "Источник",
	"reinstall": "Повторная", "reinstall_n": "да, %d-я", "unknown": "неизвестно", "campaign": "кампания",
	"installs_for": "Установки за", "total": "Всего", "reinst_short": "переуст.", "suspect": "подозрит.",
	"platforms": "Платформы", "sources": "Источники", "countries": "Страны", "more": "ещё",
	"digest": "Аналитика", "for": "за", "active": "Активные", "avg7": "ср. 7 дн.", "new": "Новые",
	"installs": "установки", "reinstalls": "переуст.", "first_visit": "первый визит веб", "signups": "регистрации",
	"installs_line": "Установки", "retention": "Удержание", "cohort": "когорты", "of": "из", "money": "Деньги",
	"purchases": "покупки", "subs": "подписки", "renewed": "продлены", "canceled": "отменены", "refunds": "возвраты",
	"key": "Ключевые", "people": "чел.", "sessions": "Сессии", "median": "медиана", "min": "мин",
	"crash_free": "без падений", "users": "польз.", "versions": "Версии", "older": "старше",
	"archive_missing": "архив за %s не выгружен", "archive_missing_n": "архив не выгружен за %d дн. (с %s)", "dropped": "выброшено сервером",
	"views": "Просмотры", "visitors": "посетители", "pages": "Страницы", "referrers": "Откуда", "no_data": "событий нет",
}

var labelsEN = map[string]string{
	"install": "Install", "platform": "Platform", "country": "Country", "source": "Source",
	"reinstall": "Reinstall", "reinstall_n": "yes, #%d", "unknown": "unknown", "campaign": "campaign",
	"installs_for": "Installs", "total": "Total", "reinst_short": "reinst.", "suspect": "suspect",
	"platforms": "Platforms", "sources": "Sources", "countries": "Countries", "more": "more",
	"digest": "Analytics", "for": "for", "active": "Active", "avg7": "7-day avg", "new": "New",
	"installs": "installs", "reinstalls": "reinst.", "first_visit": "first web visits", "signups": "sign-ups",
	"installs_line": "Installs", "retention": "Retention", "cohort": "cohort", "of": "of", "money": "Money",
	"purchases": "purchases", "subs": "subscriptions", "renewed": "renewed", "canceled": "canceled", "refunds": "refunds",
	"key": "Key", "people": "people", "sessions": "Sessions", "median": "median", "min": "min",
	"crash_free": "crash-free", "users": "users", "versions": "Versions", "older": "older",
	"archive_missing": "archive of %s not uploaded", "archive_missing_n": "archive not uploaded for %d days (since %s)", "dropped": "dropped by the server",
	"views": "Views", "visitors": "visitors", "pages": "Pages", "referrers": "From", "no_data": "no events",
}
