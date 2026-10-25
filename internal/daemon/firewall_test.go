package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeIptables keeps the rules of one chain.
type fakeIptables struct {
	rules []string
	calls []string
	fail  bool // no iptables at all
}

func (f *fakeIptables) run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args[0])
	if f.fail {
		return errors.New("iptables: not found")
	}
	rule := strings.Join(args[1:], " ")
	switch args[0] {
	case "-C":
		for _, r := range f.rules {
			if r == rule {
				return nil
			}
		}
		return errors.New("no such rule")
	case "-I":
		f.rules = append([]string{rule}, f.rules...)
	}
	return nil
}

func withIptables(t *testing.T, f *fakeIptables) {
	old := iptables
	iptables = f.run
	t.Cleanup(func() { iptables = old })
}

func TestEnsureMetadataBlocked(t *testing.T) {
	f := &fakeIptables{}
	withIptables(t, f)
	if ensureMetadataBlocked(context.Background()) != nil || len(f.rules) != 1 || !strings.Contains(f.rules[0], "169.254.169.254/32") {
		t.Fatalf("rule not added: %q", f.rules)
	}
	ensureMetadataBlocked(context.Background())
	if len(f.rules) != 1 {
		t.Fatalf("rule added twice: %q", f.rules)
	}
	f.rules = nil // docker recreated the chain
	ensureMetadataBlocked(context.Background())
	if len(f.rules) != 1 {
		t.Fatalf("rule not restored: %q", f.rules)
	}
}

func TestEnsureMetadataBlockedWithoutIptables(t *testing.T) {
	f := &fakeIptables{fail: true}
	withIptables(t, f)
	if ensureMetadataBlocked(context.Background()) == nil {
		t.Fatal("reported success without iptables")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	keepMetadataBlocked(ctx, quiet) // returns once ctx is done
}
