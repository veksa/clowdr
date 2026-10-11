package monitor

import (
	"strings"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo(strings.NewReader(`MemTotal:        3996708 kB
MemAvailable:    2516582 kB
SwapCached:            0 kB
SwapTotal:       4194300 kB
SwapFree:        4100000 kB
HugePages_Total:       0
`))
	for k, want := range map[string]int64{
		"MemTotal": 3996708 << 10, "MemAvailable": 2516582 << 10,
		"SwapTotal": 4194300 << 10, "SwapFree": 4100000 << 10,
	} {
		if m[k] != want {
			t.Errorf("%s = %d, want %d", k, m[k], want)
		}
	}
}
