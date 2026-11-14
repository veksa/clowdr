package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/store"
)

type memNPM map[string]store.NPMToken

func (m memNPM) NPMToken(id string) (store.NPMToken, error) {
	t, ok := m[id]
	if !ok {
		return t, store.ErrNotFound
	}
	return t, nil
}

func TestNPMTokens(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	secret, rec := NewNPM("laptop", []string{"npm:read:@acme/*"}, now, 0, nil)
	if !strings.HasPrefix(secret, "clwn_"+rec.ID+"_") || rec.Hash == "" || strings.Contains(rec.Hash, secret) {
		t.Fatalf("%s %+v", secret, rec)
	}
	if !rec.Expires.Equal(now.Add(NPMDefaultTTL)) {
		t.Fatalf("default lifetime: %v", rec.Expires)
	}
	ts := memNPM{rec.ID: rec}
	if got, err := CheckNPM(ts, secret, now); err != nil || got.Name != "laptop" {
		t.Fatal(got, err)
	}
	if _, err := CheckNPM(ts, secret, rec.Expires); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token accepted")
	}
	for _, bad := range []string{"", "clwn_", "clwn_" + rec.ID, secret + "x", strings.Replace(secret, "clwn_", "clw_", 1)} {
		if _, err := CheckNPM(ts, bad, now); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("accepted %q", bad)
		}
	}

	// Neither kind works where the other belongs.
	if _, err := Check(memTokens{rec.ID: {ID: rec.ID, Hash: rec.Hash}}, secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("an npm token passed the API check")
	}
	admin, arec := New("", "", "admin", true)
	if _, err := CheckNPM(memNPM{arec.ID: {ID: arec.ID, Hash: arec.Hash, Expires: now.Add(time.Hour)}}, admin, now); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("an admin token passed the npm check")
	}

	taken := map[string]bool{}
	_, r2 := NewNPM("x", []string{"plugins:read"}, now, time.Hour, func(id string) bool {
		if len(taken) == 0 {
			taken[id] = true
			return true
		}
		return false
	})
	if taken[r2.ID] {
		t.Fatal("a taken id was used")
	}
}

func TestNPMScopes(t *testing.T) {
	for _, s := range []string{"npm:read:@acme/*", "npm:publish:@acme/tool", "plugins:read", "npm:read:@a.b/c-d"} {
		if err := ValidScope(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "npm:read", "npm:read:*", "npm:read:@acme", "npm:read:lodash", "npm:read:@acme/**", "npm:read:@Acme/x",
		"npm:write:@acme/*", "plugins:write", "npm:read:@acme/../x", "admin"} {
		if ValidScope(s) == nil {
			t.Errorf("%q accepted", s)
		}
	}

	r := NPMRights{Scopes: []string{"npm:read:@acme/*", "npm:publish:@acme/tool", "npm:publish:@solo/one"}}
	cases := []struct {
		name          string
		read, publish bool
	}{
		{"@acme/tool", true, true},
		{"@acme/other", true, false},
		{"@solo/one", true, true}, // publish implies read
		{"@solo/two", false, false},
		{"@acme", false, false},
		{"@acmex/tool", false, false},
		{"@other/tool", false, false},
		{"acme/tool", false, false},
		{"tool", false, false},
		{"@acme/Tool", false, false},
		{"@acme/tool/x", false, false},
	}
	for _, c := range cases {
		if r.CanRead(c.name) != c.read || r.CanPublish(c.name) != c.publish {
			t.Errorf("%s: read %v publish %v", c.name, r.CanRead(c.name), r.CanPublish(c.name))
		}
	}
	if r.Plugins() || !(NPMRights{Scopes: []string{"plugins:read"}}).Plugins() {
		t.Error("plugins:read")
	}
}
