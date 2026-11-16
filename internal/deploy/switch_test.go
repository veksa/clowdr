package deploy

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/spec"
)

var serverLine = regexp.MustCompile(`server (\S+-r\d+-\d+:\d+) resolve`)

// upstreamServers lists the container servers of a rendered config, in order.
func upstreamServers(conf string) []string {
	var out []string
	for _, m := range serverLine.FindAllStringSubmatch(conf, -1) {
		out = append(out, m[1])
	}
	return out
}

// reloads records the upstream servers and whether a site is served at every nginx reload.
type reload struct {
	servers []string
	sites   bool
	at      time.Time
}

func (f *fixture) recordReloads(project string) *[]reload {
	var rs []reload
	f.nginx.OnReload = func() {
		conf := f.confOf(project)
		rs = append(rs, reload{upstreamServers(conf), strings.Contains(conf, "server_name"), time.Now()})
	}
	return &rs
}

// A start-first switch never reloads nginx straight from the old containers to new ones: nginx
// resolves a name it did not have only after the reload, and until then the upstream would
// have no address at all. The new names come in next to the old, which keep serving, and take
// the traffic alone in a later reload, at least ResolveSettle after.
func TestStartFirstSwitchResolvesNewNamesNextToOld(t *testing.T) {
	ctx := context.Background()
	ops := []struct {
		name  string
		setup func(f *fixture) error
		run   func(f *fixture) error
		old   []string
		new   []string
	}{
		{"deploy", nil, func(f *fixture) error { _, err := f.deploy(t, "v2", serverSpec()); return err },
			[]string{"shop-server-r1-1:3000"}, []string{"shop-server-r2-1:3000"}},
		{"restart", nil, func(f *fixture) error { _, err := f.e.Restart(ctx, "shop", "server", "test", nop); return err },
			[]string{"shop-server-r1-1:3000"}, []string{"shop-server-r2-1:3000"}},
		{"scale", nil, func(f *fixture) error { _, err := f.e.Scale(ctx, "shop", "server", 2, "test", nop); return err },
			[]string{"shop-server-r1-1:3000"}, []string{"shop-server-r2-1:3000", "shop-server-r2-2:3000"}},
		{"rollback", func(f *fixture) error { _, err := f.deploy(t, "v2", serverSpec()); return err },
			func(f *fixture) error { _, err := f.e.Rollback(ctx, "shop", "server", 0, "test", nop); return err },
			[]string{"shop-server-r2-1:3000"}, []string{"shop-server-r3-1:3000"}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
				t.Fatal(err)
			}
			if op.setup != nil {
				if err := op.setup(f); err != nil {
					t.Fatal(err)
				}
			}
			f.e.ResolveSettle = 30 * time.Millisecond
			rs := f.recordReloads("shop")
			if err := op.run(f); err != nil {
				t.Fatal(err)
			}
			f.e.WaitDrains()
			got := *rs
			if len(got) != 2 {
				t.Fatalf("want 2 reloads, got %d: %+v", len(got), got)
			}
			if want := append(slices.Clone(op.old), op.new...); !slices.Equal(got[0].servers, want) {
				t.Fatalf("first reload: want %v, got %v", want, got[0].servers)
			}
			if !slices.Equal(got[1].servers, op.new) {
				t.Fatalf("second reload: want %v, got %v", op.new, got[1].servers)
			}
			if d := got[1].at.Sub(got[0].at); d < f.e.ResolveSettle {
				t.Fatalf("old names left %s after the new came in, want at least %s", d, f.e.ResolveSettle)
			}
		})
	}
}

// Stop-first has nothing that still serves: no overlap, one reload.
func TestStopFirstSwitchHasNoOverlap(t *testing.T) {
	f := newFixture(t)
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	rs := f.recordReloads("shop")
	s := serverSpec()
	s.Switch = spec.SwitchStopFirst
	if _, err := f.deploy(t, "v2", s); err != nil {
		t.Fatal(err)
	}
	if got := *rs; len(got) != 1 || !slices.Equal(got[0].servers, []string{"shop-server-r2-1:3000"}) {
		t.Fatalf("reloads: %+v", got)
	}
}

// A failed switch takes the new names out again and leaves the old serving.
func TestOverlapRevertedWhenNginxRejects(t *testing.T) {
	f := newFixture(t)
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	f.nginx.TestErr = errors.New("rejected")
	if _, err := f.deploy(t, "v2", serverSpec()); err == nil {
		t.Fatal("deploy passed a rejected config")
	}
	f.nginx.TestErr = nil
	if got := upstreamServers(f.conf(t)); !slices.Equal(got, []string{"shop-server-r1-1:3000"}) {
		t.Fatalf("servers after a failed switch: %v", got)
	}
	if got := f.d.RunningWith("shop-server"); len(got) != 1 || !strings.Contains(got[0], "r1") {
		t.Fatalf("running: %v", got)
	}
	if len(f.e.overlaps) != 0 {
		t.Fatalf("overlap left: %v", f.e.overlaps)
	}
}

// A rename starts a new upstream for nginx: the new containers come in under the new name in
// a config without sites first, the sites move in a later reload.
func TestRenameResolvesNewUpstreamsBeforeTheSwitch(t *testing.T) {
	f := newFixture(t)
	if _, err := f.deploy(t, "v1", serverSpec()); err != nil {
		t.Fatal(err)
	}
	f.e.ResolveSettle = 30 * time.Millisecond
	rs := f.recordReloads("store")
	if err := f.e.RenameProject(context.Background(), "shop", "store", "test", nil); err != nil {
		t.Fatal(err)
	}
	f.e.WaitDrains()
	got := *rs
	if len(got) != 2 {
		t.Fatalf("want 2 reloads, got %+v", got)
	}
	want := []string{"store-server-r1-1:3000"}
	if !slices.Equal(got[0].servers, want) || got[0].sites {
		t.Fatalf("first reload: %+v", got[0])
	}
	if !slices.Equal(got[1].servers, want) || !got[1].sites {
		t.Fatalf("second reload: %+v", got[1])
	}
	if d := got[1].at.Sub(got[0].at); d < f.e.ResolveSettle {
		t.Fatalf("sites moved %s after the upstreams, want at least %s", d, f.e.ResolveSettle)
	}
}
