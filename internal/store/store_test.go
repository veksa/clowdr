package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestServicesAndReleases(t *testing.T) {
	s := open(t)
	if err := s.PutProject(Project{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProject(Project{Name: "ab"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "ab"} {
		if _, err := s.UpdateService(p, "web", func(v *Service) error { v.Spec.Port = 80; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := s.Services("a")
	if len(vs) != 1 || vs[0].Project != "a" {
		t.Fatalf("prefix leak: %+v", vs)
	}
	all, _ := s.Services("")
	if len(all) != 2 {
		t.Fatal(len(all))
	}
	for i := 0; i < 300; i++ { // crosses a byte boundary in the big-endian key
		r := &Release{Project: "a", Service: "web", Status: StatusActive}
		if err := s.CreateRelease(r); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := s.Releases("a", "web")
	if len(rs) != 300 || rs[0].ID < rs[1].ID {
		t.Fatalf("releases order: %d %d", rs[0].ID, rs[1].ID)
	}
	if rs2, _ := s.Releases("ab", "web"); len(rs2) != 0 {
		t.Fatal("release prefix leak")
	}
	if err := s.PutToken(Token{ID: "t1", Project: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Service("a", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatal("service not deleted")
	}
	if _, err := s.Token("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("token not deleted")
	}
	if rs, _ := s.Releases("a", "web"); len(rs) != 0 {
		t.Fatal("releases not deleted")
	}
	if _, err := s.Service("ab", "web"); err != nil {
		t.Fatal("other project touched")
	}
}
