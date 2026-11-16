package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
)

// A lost disk: a new server with only the config and the secrets restores the index and the
// rollups from the archive (snapshot, rollups, raw days after the snapshot) and answers every
// rollup query as the old one did.
func TestRestoreGivesTheSameAnswers(t *testing.T) {
	f := newFakeS3(t)
	f.page = 2                                           // several list pages
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC) // a Sunday: the snapshot goes up
	e := newEnv(t, now)
	e.m.HTTP = f.srv.Client()
	secrets := map[string]string{SecretS3Key: "key", SecretS3Secret: f.secret, SecretS3Prefix: "analytics/00ff"}
	cfg := config.AnalyticsConf{Archive: &config.ArchiveConf{Endpoint: f.srv.URL, Region: "auto", Bucket: "bucket"}}
	e.configure(cfg, secrets)
	lines := fixtureLines(t)
	e.feed(time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC), lines[:15]...)
	e.feed(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), lines[15:]...)
	p := e.project()
	if _, err := p.Recompute([]string{"2026-11-10", "2026-11-11", "2026-11-12"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ArchiveDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := string(mustDecompress(t, f.objects["analytics/00ff/v1/shop/state/2026-11-15.ndjson.zst"]))
	if !strings.Contains(snap, `"b":"groups"`) || !strings.Contains(snap, `"b":"catalog"`) {
		t.Fatalf("the snapshot lacks groups or the catalog:\n%s", snap)
	}

	specs := []Spec{
		{Kind: "trends", Events: []string{"episode_viewed", "any"}, Math: "uniques"},
		{Kind: "trends", Events: []string{"episode_viewed"}, Breakdown: "pf"},
		{Kind: "retention", Period: "day"},
		{Kind: "retention", Period: "day", Breakdown: "first_source"},
		{Kind: "stickiness", Event: "any"},
		{Kind: "lifecycle", Event: "any", Interval: "day"},
		{Kind: "installs"},
		{Kind: "catalog"},
	}
	answers := func(e *env) []string {
		var out []string
		for _, s := range specs {
			s.Since, s.Until = "2026-11-10", "2026-11-14T23:59:59Z"
			r := e.query(s)
			r.Window = Window{}
			b, _ := json.Marshal(r)
			out = append(out, string(b))
		}
		return out
	}
	want := answers(e)

	e2 := newEnv(t, now) // the same clock: retention runs up to now
	e2.m.HTTP = f.srv.Client()
	e2.configure(cfg, secrets)
	e2.m.Tick(context.Background()) // finds the index empty and restores it
	p2 := e2.project()
	if p2.state.Empty() {
		t.Fatal("the tick did not restore the index")
	}
	got := answers(e2)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s:\nwant %s\ngot  %s", specs[i].Kind, want[i], got[i])
		}
	}
	// The restored numbering goes on after the restored actors and groups.
	p2.state.Update(func(tx *Tx) error {
		old, _ := tx.Actor("u:870")
		a, _ := tx.EnsureActor("d:brand-new", nil)
		if a.ID <= old.ID {
			t.Errorf("new actor %d not after %d", a.ID, old.ID)
		}
		return nil
	})
	// A second restore refuses a non-empty index; a second tick does not try again.
	if _, err := p2.Restore(context.Background(), nil); err == nil {
		t.Error("restore over a live index")
	}
	lists := f.lists
	e2.now = e2.now.Add(2 * time.Hour)
	e2.m.Tick(context.Background())
	if f.lists != lists {
		t.Error("a tick looked into the archive although the index is there")
	}
}

func mustDecompress(t *testing.T, b []byte) []byte {
	t.Helper()
	raw, err := decompress(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPrivateAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true,
		"169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true, "::1": true, "fd00::1": true,
		"fe80::1": true, "::ffff:127.0.0.1": true, "64:ff9b::a9fe:a9fe": true,
		"1.1.1.1": false, "8.8.8.8": false, "2606:4700::1111": false,
	} {
		if got := privateAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

// The archive endpoint is set by a project token: clowdrd must not dial the server's own
// network for it, whether the URL names the address or a name that resolves there.
func TestArchiveEndpointMustBePublic(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := strings.Cut(hostport, ":")
	var allow []string
	c := guardedClient(func() []string { return allow }, 5*time.Second)
	for _, u := range []string{srv.URL, "http://localhost:" + port} {
		resp, err := c.Get(u)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: dialled", u)
		}
		if !errors.Is(err, ErrPrivateEndpoint) {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if hits != 0 {
		t.Fatal("the server was reached")
	}
	allow = []string{hostport}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("an allowed endpoint: %v", err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Fatal("the allowed endpoint was not reached")
	}

	// Through the manager: the archive refuses the endpoint until the admin allows it.
	f := newFakeS3(t)
	e := newEnv(t, time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC))
	fhost := strings.TrimPrefix(f.srv.URL, "http://")
	e.m.AllowPrivate = func() []string { return allow }
	e.configure(config.AnalyticsConf{Archive: &config.ArchiveConf{Endpoint: f.srv.URL, Bucket: "bucket", Region: "auto"}},
		map[string]string{SecretS3Key: "key", SecretS3Secret: f.secret, SecretS3Prefix: "p"})
	e.feed(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), fixtureLines(t)[:3]...)
	err = e.project().ArchiveDue(context.Background())
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("a private endpoint: %v", err)
	}
	allow = []string{fhost}
	if err := e.project().ArchiveDue(context.Background()); err != nil {
		t.Fatalf("an allowed endpoint: %v", err)
	}
}

func TestRollupCacheIsBoundedByBytes(t *testing.T) {
	defer SetRollupCacheLimit(0)
	SetRollupCacheLimit(1000)
	mod := time.Unix(1, 0)
	for i := 0; i < 10; i++ {
		rollCache.put("/x/"+itoa(i), mod, &DayRollup{}, 300)
	}
	n, used := rollCache.stats()
	if used > 1000 || n != 3 {
		t.Fatalf("%d entries, %d bytes", n, used)
	}
	if rollCache.get("/x/9", mod) == nil || rollCache.get("/x/0", mod) != nil {
		t.Fatal("not least recently used out first")
	}
	rollCache.put("/x/huge", mod, &DayRollup{}, 5000)
	if rollCache.get("/x/huge", mod) != nil {
		t.Fatal("an entry over the whole budget was kept")
	}
	if rollCache.get("/x/9", time.Unix(2, 0)) != nil {
		t.Fatal("a changed file was served from the cache")
	}
}

// The day pass reads one day at a time and picks lines by their ts; a record written with
// spaces in its JSON still counts.
func TestRecomputeReadsLooseJSON(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	e.configure(config.AnalyticsConf{}, nil)
	e.feed(time.Time{},
		`{"$":"analytics","v":1,"id":"a","ts":"2026-11-15T10:00:00Z","e":"$pageview","u":"1"}`,
		`{"$":"analytics", "v":1, "id":"b", "ts": "2026-11-15T11:00:00Z", "e":"$pageview", "u":"2"}`,
		`{"$":"analytics","v":1,"id":"c","ts":"2026-11-16T00:30:00+02:00","e":"$pageview","u":"3"}`,
	)
	if _, err := e.project().Recompute([]string{"2026-11-15"}); err != nil {
		t.Fatal(err)
	}
	r, _ := LoadRollup(e.project().dir, "2026-11-15")
	if r == nil || r.Events["$pageview"].N != 3 {
		t.Fatalf("rollup %+v", r)
	}
}
