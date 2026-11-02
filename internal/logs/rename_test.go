package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenameProject(t *testing.T) {
	s := newStore(t)
	app := Key{"shop", KindApp, "api"}
	trace := Key{"shop", KindTrace, "api"}
	neighbour := Key{"shop2", KindApp, "api"}
	t0 := time.Now().Add(-time.Minute)
	s.Append(app, Line{T: t0.UnixNano(), L: "flushed"})
	if err := s.Flush(app); err != nil {
		t.Fatal(err)
	}
	s.Append(app, Line{T: t0.Add(time.Second).UnixNano(), L: "buffered"})
	s.Append(trace, Line{T: t0.UnixNano(), L: "a trace"})
	s.Append(neighbour, Line{T: t0.UnixNano(), L: "neighbour"})

	s.RenameProject("shop", "store")

	newApp := Key{"store", KindApp, "api"}
	if got := strings.Join(lines(s, newApp, Query{}), ","); got != "flushed,buffered" {
		t.Fatalf("app lines %q", got)
	}
	if got := strings.Join(lines(s, Key{"store", KindTrace, "api"}, Query{}), ","); got != "a trace" {
		t.Fatalf("trace lines %q", got)
	}
	if got := lines(s, app, Query{}); len(got) != 0 {
		t.Fatalf("lines left under the old name: %v", got)
	}
	if got := strings.Join(lines(s, neighbour, Query{}), ","); got != "neighbour" {
		t.Fatalf("neighbour %q", got)
	}

	// A draining container still writes under the old name; the second pass lands in the same
	// hourly segment and both read back.
	if err := s.Append(newApp, Line{T: t0.Add(2 * time.Second).UnixNano(), L: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(newApp); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(app, Line{T: t0.Add(3 * time.Second).UnixNano(), L: "late"}); err != nil {
		t.Fatal(err)
	}
	s.RenameProject("shop", "store")
	if got := strings.Join(lines(s, newApp, Query{}), ","); got != "flushed,buffered,new,late" {
		t.Fatalf("after the second pass %q", got)
	}
	if _, err := os.Stat(filepath.Join(s.DataDir, "shop")); !os.IsNotExist(err) {
		t.Fatalf("old directory: %v", err)
	}
}
