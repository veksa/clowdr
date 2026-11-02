package store

import (
	"errors"
	"slices"
	"testing"

	"github.com/veksa/clowdr/internal/spec"
)

func TestRenameProject(t *testing.T) {
	s := open(t)
	for _, p := range []string{"shop", "shop2", "shared"} {
		if err := s.PutProject(Project{Name: p}); err != nil {
			t.Fatal(err)
		}
	}
	mustService := func(project, name string, sp spec.Service) {
		t.Helper()
		if _, err := s.UpdateService(project, name, func(v *Service) error { v.Spec = sp; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	mustService("shop", "web", spec.Service{AcceptFrom: []string{"shared"}})
	mustService("shop2", "web", spec.Service{})
	mustService("shared", "redis", spec.Service{ExposeTo: []string{"shop", "shop2"}})
	r := Release{Project: "shop", Service: "web", Image: "old", Containers: []string{"c"}}
	if err := s.CreateRelease(&r); err != nil {
		t.Fatal(err)
	}
	other := Release{Project: "shared", Service: "redis", Image: "redis", Spec: spec.Service{ExposeTo: []string{"shop"}}}
	if err := s.CreateRelease(&other); err != nil {
		t.Fatal(err)
	}
	s.PutToken(Token{ID: "a", Project: "shop"})
	s.PutToken(Token{ID: "b", Project: "shop2"})
	s.PutToken(Token{ID: "c", Admin: true})
	s.Own("shop", "d1", "d2")
	s.Own("shop2", "d3")

	if err := s.RenameProject("shop", "store", func(r *Release) { r.Image = "new" }); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Project("shop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old project: %v", err)
	}
	if p, err := s.Project("store"); err != nil || p.RenamedFrom != "shop" {
		t.Fatalf("project %+v, %v", p, err)
	}
	if err := s.FinishRename("store"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Project("store"); p.RenamedFrom != "" {
		t.Fatalf("mark left: %+v", p)
	}
	if vs, _ := s.Services("shop"); len(vs) != 0 {
		t.Fatalf("services under the old name: %+v", vs)
	}
	if v, err := s.Service("store", "web"); err != nil || v.Project != "store" || !slices.Equal(v.Spec.AcceptFrom, []string{"shared"}) {
		t.Fatalf("service %+v, %v", v, err)
	}
	if rs, _ := s.Releases("shop", "web"); len(rs) != 0 {
		t.Fatalf("releases under the old name: %+v", rs)
	}
	if got, err := s.Release("store", "web", r.ID); err != nil || got.Project != "store" || got.Image != "new" {
		t.Fatalf("release %+v, %v", got, err)
	}
	// The name in other projects' specs follows; a name that only starts the same does not.
	if v, _ := s.Service("shared", "redis"); !slices.Equal(v.Spec.ExposeTo, []string{"store", "shop2"}) {
		t.Fatalf("expose_to %v", v.Spec.ExposeTo)
	}
	if got, _ := s.Release("shared", "redis", other.ID); !slices.Equal(got.Spec.ExposeTo, []string{"store"}) || got.Image != "redis" {
		t.Fatalf("other release %+v", got)
	}
	if v, err := s.Service("shop2", "web"); err != nil || v.Project != "shop2" {
		t.Fatalf("neighbour %+v, %v", v, err)
	}
	if tok, _ := s.Token("a"); tok.Project != "store" {
		t.Fatalf("token %+v", tok)
	}
	if tok, _ := s.Token("b"); tok.Project != "shop2" {
		t.Fatalf("neighbour's token %+v", tok)
	}
	for project, want := range map[string]map[string]bool{
		"store": {"d1": true, "d2": true, "d3": false},
		"shop":  {"d1": false, "d2": false},
		"shop2": {"d3": true},
	} {
		for d, w := range want {
			if ok, _ := s.Owns(project, d); ok != w {
				t.Errorf("%s owns %s: %v", project, d, ok)
			}
		}
	}

	// A taken name leaves everything as it was.
	if err := s.RenameProject("store", "shop2", nil); err == nil {
		t.Fatal("renamed over an existing project")
	}
	if _, err := s.Service("store", "web"); err != nil {
		t.Fatalf("a failed rename changed state: %v", err)
	}
}
