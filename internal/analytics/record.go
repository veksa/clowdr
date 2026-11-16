// Package analytics keeps product analytics a service writes to stdout as JSON records starting
// with {"$":"analytics" (docs/analytics.md): an index of people, devices and install keys,
// daily rollups with exact bitmaps of actors, a daily archive in the project's S3 bucket, queries
// (trends, funnels, retention, ...) and a daily digest to Telegram.
package analytics

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mark starts every analytics record; logs.AnalyticsMark is the same string.
const Mark = `{"$":"analytics"`

// Version is the record format this package reads.
const Version = 1

var eventNameRe = regexp.MustCompile(`^[$a-z][a-z0-9_]{1,47}$`)

// ValidEventName reports whether s is a valid event name.
func ValidEventName(s string) bool { return eventNameRe.MatchString(s) }

// Apps and platforms the format names; others are accepted on ingest and reported by `check`.
var (
	knownApps = set("mobile", "tv", "watch", "wear", "xr", "dashboard", "admin", "landing", "web", "bot", "mcp", "server", "desktop")
	knownPF   = set("ios", "android", "tvos", "watchos", "wearos", "visionos", "web", "telegram", "server", "macos", "windows")
	knownFF   = set("phone", "tablet", "tv", "watch", "xr", "desktop")
)

func set(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// Reserved event names clowdr acts on.
const (
	EventFirstOpen      = "$first_open"
	EventIdentify       = "$identify"
	EventSessionEnd     = "$session_end"
	EventPageview       = "$pageview"
	EventClientError    = "$client_error"
	EventAccountDeleted = "account_deleted"
)

// passive events do not make an actor active for the day: they happen to a person, not by one
// (a push arrived, a store renewed a subscription, an account was removed).
var passive = set("$set", "$push_received", "subscription_renewed", "subscription_expired", "subscription_canceled",
	"subscription_refunded", "purchase_refunded", EventAccountDeleted)

// Record is one analytics event, format v1.
type Record struct {
	Mark string         `json:"$"`
	V    int            `json:"v"`
	ID   string         `json:"id"`
	TS   string         `json:"ts"`
	E    string         `json:"e"`
	U    flexString     `json:"u,omitempty"`
	D    string         `json:"d,omitempty"`
	S    string         `json:"s,omitempty"`
	App  string         `json:"app,omitempty"`
	PF   string         `json:"pf,omitempty"`
	FF   string         `json:"ff,omitempty"`
	AV   string         `json:"av,omitempty"`
	B    string         `json:"b,omitempty"`
	OS   string         `json:"os,omitempty"`
	LC   string         `json:"lc,omitempty"`
	CC   string         `json:"cc,omitempty"`
	Env  string         `json:"env,omitempty"`
	Src  string         `json:"src,omitempty"`
	DM   map[string]any `json:"dm,omitempty"`
	P    map[string]any `json:"p,omitempty"`
	Set  map[string]any `json:"set,omitempty"`
	Once map[string]any `json:"once,omitempty"`
	G    map[string]any `json:"g,omitempty"`
	IK   string         `json:"ik,omitempty"`

	t time.Time
}

// flexString takes a JSON string or number: user ids are strings in the format, but a number
// written by mistake should not lose the event.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// User is the user id, "" for an anonymous event.
func (r *Record) User() string { return string(r.U) }

// Time is the event time.
func (r *Record) Time() time.Time { return r.t }

// Day is the UTC day of the event, YYYY-MM-DD.
func (r *Record) Day() string { return r.t.UTC().Format(DayLayout) }

// DayLayout formats days in rollups, archive keys and queries.
const DayLayout = "2006-01-02"

// Dim returns a dimension of the event: a built-in one (app, pf, ff, av, cc, lc, src, env) or one
// of dm.
func (r *Record) Dim(k string) string {
	switch k {
	case "app":
		return r.App
	case "pf":
		return r.PF
	case "ff":
		return r.FF
	case "av":
		return r.AV
	case "cc":
		return r.CC
	case "lc":
		return r.LC
	case "src":
		return r.Src
	case "env":
		return r.Env
	case "event":
		return r.E
	}
	return str(r.DM[k])
}

// Prop returns a property from p, or "" with ok false.
func (r *Record) Prop(k string) (any, bool) {
	v, ok := r.P[k]
	return v, ok
}

// BuiltinDims are the dimensions every rollup keeps for every event.
var BuiltinDims = []string{"app", "pf", "ff", "av", "cc"}

// str renders a JSON scalar the way dimensions and filters compare it.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// num reads a JSON number (or a numeric string).
func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Parse decodes one stored line. It accepts what clowdr can count: the mark, v 1, an id, a
// time and a valid name; everything else is checked by Validate.
func Parse(line string) (Record, error) {
	var r Record
	if !strings.HasPrefix(line, Mark) {
		return r, fmt.Errorf("not an analytics record")
	}
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return r, fmt.Errorf("bad JSON: %v", err)
	}
	if r.V != Version {
		return r, fmt.Errorf("unsupported version %d", r.V)
	}
	if r.ID == "" {
		return r, fmt.Errorf("no id")
	}
	t, err := time.Parse(time.RFC3339Nano, r.TS)
	if err != nil {
		return r, fmt.Errorf("bad ts %q", r.TS)
	}
	r.t = t.UTC()
	if !ValidEventName(r.E) {
		return r, fmt.Errorf("bad event name %q", r.E)
	}
	return r, nil
}

// Validate lists what is wrong with a parsed record against format v1 beyond what Parse needs.
// Ingest keeps such records; `clowctl analytics check` reports them.
func Validate(r *Record) []string {
	var out []string
	if r.User() == "" && r.D == "" {
		out = append(out, "neither u nor d: the event has no actor")
	}
	if r.App != "" && !knownApps[r.App] {
		out = append(out, fmt.Sprintf("unknown app %q", r.App))
	}
	if r.PF != "" && !knownPF[r.PF] {
		out = append(out, fmt.Sprintf("unknown pf %q", r.PF))
	}
	if r.FF != "" && !knownFF[r.FF] {
		out = append(out, fmt.Sprintf("unknown ff %q", r.FF))
	}
	if r.Src != "" && r.Src != "client" && r.Src != "server" && r.Src != "posthog" {
		out = append(out, fmt.Sprintf("src %q is neither client nor server", r.Src))
	}
	if r.CC != "" && (len(r.CC) != 2 || strings.ToUpper(r.CC) != r.CC) {
		out = append(out, fmt.Sprintf("cc %q is not ISO-2 upper case", r.CC))
	}
	if r.LC != "" && len(r.LC) != 2 {
		out = append(out, fmt.Sprintf("lc %q is not two letters", r.LC))
	}
	if len(r.DM) > 5 { // four from the catalogue, plus suspect added by the server
		out = append(out, fmt.Sprintf("dm has %d keys, at most 4 (+suspect)", len(r.DM)))
	}
	for k, v := range r.DM {
		s, ok := v.(string)
		if !ok {
			out = append(out, fmt.Sprintf("dm.%s is not a string", k))
		} else if len(s) > 64 {
			out = append(out, fmt.Sprintf("dm.%s is longer than 64", k))
		}
	}
	for k, v := range r.P {
		switch x := v.(type) {
		case string:
			if len(x) > 200 {
				out = append(out, fmt.Sprintf("p.%s is longer than 200", k))
			}
		case float64, bool, nil:
		default:
			out = append(out, fmt.Sprintf("p.%s is not a number, boolean or string", k))
		}
	}
	if r.IK != "" && r.E != EventFirstOpen {
		out = append(out, "ik outside $first_open")
	}
	if r.E == EventIdentify && r.User() == "" {
		out = append(out, "$identify without u")
	}
	return out
}

// reinstallHint is set by a client that found its install key but no first-run marker (iOS
// keychain survives an uninstall), so the install is a reinstall even if clowdr never saw the key.
func (r *Record) reinstallHint() bool {
	v, ok := r.P["reinstall_hint"]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return (isBool && b) || str(v) == "1" || str(v) == "true"
}

// suspect marks a $first_open the server found suspicious (many from one address).
func (r *Record) suspect() bool { return str(r.DM["suspect"]) == "1" }

// anon is the device an $identify links: p.anon, else the event's own device.
func (r *Record) anon() string {
	if a := str(r.P["anon"]); a != "" {
		return a
	}
	return r.D
}
