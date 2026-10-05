package ingress

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeNginx struct {
	dir     string
	tests   int
	reloads int
}

// Test rejects any file containing "bad".
func (f *fakeNginx) Test(context.Context) (string, error) {
	f.tests++
	var bad bool
	filepath.Walk(f.dir, func(p string, _ os.FileInfo, _ error) error {
		b, _ := os.ReadFile(p)
		bad = bad || strings.Contains(string(b), "bad")
		return nil
	})
	if bad {
		return "emerg: bad", errors.New("exit 1")
	}
	return "ok", nil
}

func (f *fakeNginx) Reload(context.Context) error { f.reloads++; return nil }

func TestApply(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNginx{dir: dir}
	c := &Controller{Dir: dir, Nginx: n}
	ctx := context.Background()
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	if err := c.Apply(ctx, File{"conf.d/a.conf", "good v1"}); err != nil {
		t.Fatal(err)
	}
	if n.reloads != 1 {
		t.Fatal("not reloaded")
	}
	// Unchanged: no test, no reload.
	if err := c.Apply(ctx, File{"conf.d/a.conf", "good v1"}); err != nil || n.reloads != 1 || n.tests != 1 {
		t.Fatal("reloaded without changes")
	}
	// Rejected: both files restored to their previous state.
	err := c.Apply(ctx, File{"conf.d/a.conf", "good v2"}, File{"conf.d/b.conf", "bad"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if read("conf.d/a.conf") != "good v1" || read("conf.d/b.conf") != "<missing>" || n.reloads != 1 {
		t.Fatal("not restored")
	}
	// Removal.
	if err := c.Apply(ctx, File{"conf.d/a.conf", ""}); err != nil || read("conf.d/a.conf") != "<missing>" || n.reloads != 2 {
		t.Fatal("not removed")
	}
}
