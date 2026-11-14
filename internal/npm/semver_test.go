package npm

import (
	"sort"
	"testing"
)

func TestSemver(t *testing.T) {
	for _, v := range []string{"1.0.0", "0.0.1", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0+build.1", "1.0.0-rc.1+b"} {
		if !validVersion(v) {
			t.Errorf("%s refused", v)
		}
	}
	for _, v := range []string{"", "1", "1.0", "v1.0.0", "01.0.0", "1.0.0-", "1.0.0-01", "1.0.0 ", "../1.0.0", "1.0.0/x", "1.0.0-a..b"} {
		if validVersion(v) {
			t.Errorf("%q accepted", v)
		}
	}
	// semver.org §11, in order.
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.2.0", "1.10.0", "2.0.0"}
	shuffled := append([]string(nil), ordered...)
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i] > shuffled[j] }) // by text, wrong on purpose
	sort.SliceStable(shuffled, func(i, j int) bool { return compareSemver(shuffled[i], shuffled[j]) < 0 })
	for i := range ordered {
		if shuffled[i] != ordered[i] {
			t.Fatalf("order: %v", shuffled)
		}
	}
	if compareSemver("1.0.0+a", "1.0.0+b") != 0 {
		t.Error("build metadata must not order")
	}
}
