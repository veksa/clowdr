package npm

import (
	"regexp"
	"strconv"
	"strings"
)

// semverRe is semver 2.0.0 (https://semver.org), without a leading "v".
var semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

type semver struct {
	major, minor, patch uint64
	pre                 []string
}

// parseSemver parses a version; ok is false for anything that is not plain semver.
func parseSemver(s string) (semver, bool) {
	if len(s) > 256 {
		return semver{}, false
	}
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false
	}
	var v semver
	var err error
	if v.major, err = strconv.ParseUint(m[1], 10, 64); err != nil {
		return semver{}, false
	}
	if v.minor, err = strconv.ParseUint(m[2], 10, 64); err != nil {
		return semver{}, false
	}
	if v.patch, err = strconv.ParseUint(m[3], 10, 64); err != nil {
		return semver{}, false
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

func validVersion(s string) bool {
	_, ok := parseSemver(s)
	return ok
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareSemver orders versions by semver precedence; build metadata is ignored. Strings that
// are not semver sort before every version, and among themselves by text.
func compareSemver(a, b string) int {
	va, oka := parseSemver(a)
	vb, okb := parseSemver(b)
	switch {
	case !oka && !okb:
		return strings.Compare(a, b)
	case !oka:
		return -1
	case !okb:
		return 1
	}
	if c := cmpUint(va.major, vb.major); c != 0 {
		return c
	}
	if c := cmpUint(va.minor, vb.minor); c != 0 {
		return c
	}
	if c := cmpUint(va.patch, vb.patch); c != 0 {
		return c
	}
	switch {
	case len(va.pre) == 0 && len(vb.pre) == 0:
		return 0
	case len(va.pre) == 0:
		return 1 // a release is above its prereleases
	case len(vb.pre) == 0:
		return -1
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		x, y := va.pre[i], vb.pre[i]
		nx, ex := strconv.ParseUint(x, 10, 64)
		ny, ey := strconv.ParseUint(y, 10, 64)
		var c int
		switch {
		case ex == nil && ey == nil:
			c = cmpUint(nx, ny)
		case ex == nil:
			c = -1 // numeric identifiers are below alphanumeric ones
		case ey == nil:
			c = 1
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmpUint(uint64(len(va.pre)), uint64(len(vb.pre)))
}
