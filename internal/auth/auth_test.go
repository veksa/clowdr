package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/veksa/clowdr/internal/store"
)

type memTokens map[string]store.Token

func (m memTokens) Token(id string) (store.Token, error) {
	t, ok := m[id]
	if !ok {
		return t, store.ErrNotFound
	}
	return t, nil
}

func TestTokens(t *testing.T) {
	secret, rec := New("blog", "prod", "ci", false)
	ts := memTokens{rec.ID: rec}
	p, err := Check(ts, secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Admin || p.Project != "blog" || !p.CanProject("blog") || p.CanProject("shop") || p.CanProject("") {
		t.Fatalf("%+v", p)
	}
	bad := []string{
		"", "clw_", "clw_" + rec.ID, "clw_" + rec.ID + "_x", secret + "x",
		strings.Replace(secret, "clw_", "clx_", 1),
	}
	for _, b := range bad {
		if _, err := Check(ts, b); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("accepted %q", b)
		}
	}
	as, arec := New("", "", "admin", true)
	ts[arec.ID] = arec
	ap, err := Check(ts, as)
	if err != nil || !ap.CanProject("anything") {
		t.Fatal("admin")
	}
	if strings.Contains(rec.Hash, secret) {
		t.Fatal("secret stored")
	}
}

func TestDomainAllowed(t *testing.T) {
	pat := []string{"example.com", "*.example.com"}
	cases := map[string]bool{
		"example.com":         true,
		"api.example.com":     true,
		"API.Example.com":     true,
		"*.example.com":       true,
		"a.b.example.com":     false,
		"example.com.evil.io": false,
		"evilexample.com":     false,
		"example.org":        false,
		".example.com":        false,
	}
	for d, want := range cases {
		if got := DomainAllowed(pat, d); got != want {
			t.Errorf("%s: got %v", d, got)
		}
	}
	if DomainAllowed(nil, "a.com") {
		t.Error("empty patterns allow everything")
	}
}
