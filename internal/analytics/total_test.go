package analytics

import "testing"

// The total of a series whose math does not add up across buckets is a whole-range value,
// not the last bucket: distinct actors over the range, events per one of them, and the
// average daily value for dau, wau and mau. Rollups and raw records agree on it.
func TestTrendsTotalIsWholeRange(t *testing.T) {
	e := fixtureEnv(t)
	cases := []struct {
		event, math, interval string
		want                  float64
	}{
		{"any", "uniques", "day", 5},
		{"any", "uniques", "week", 5},
		{"episode_viewed", "uniques", "day", 1},
		{"episode_viewed", "per_user_avg", "day", 3},
		{"any", "dau", "day", round2(8.0 / 3)},
		{"any", "dau", "month", round2(8.0 / 3)},
		{"any", "wau", "day", 5},
		{"any", "mau", "week", 5},
	}
	for _, c := range cases {
		for _, raw := range []bool{false, true} {
			r := e.query(spec("trends", func(s *Spec) {
				s.Events = []string{c.event}
				s.Math = c.math
				s.Interval = c.interval
				if raw {
					s.Filters = []string{"p.no_such_prop!=x"}
				}
			}))
			want := "rollup"
			if raw {
				want = "raw"
			}
			if r.Source != want {
				t.Fatalf("%s/%s/%s: source %s, want %s", c.event, c.math, c.interval, r.Source, want)
			}
			if kind := r.Series[0].TotalKind; (kind == "average") != (c.math == "dau" || c.math == "wau" || c.math == "mau") {
				t.Errorf("%s/%s: total kind %q", c.math, r.Source, kind)
			}
			if got := r.Series[0].Total; got != c.want {
				t.Errorf("%s/%s/%s from %s: total %v, want %v (points %v)", c.event, c.math, c.interval, r.Source, got, c.want, points(r.Series[0]))
			}
		}
	}
}
