package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRenameProjectEndpoint(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	token := e.projectClient(t, "shop", "example.com")
	rename := func(c *Client, from, to string) error {
		_, err := c.Stream(ctx, "POST", Path("projects", from, "rename"), RenameProjectRequest{Name: to}, func(Event) {})
		return err
	}

	var se *StatusError
	if err := rename(token, "shop", "store"); !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("a project token must not rename: %v", err)
	}
	if err := rename(e.admin, "nope", "store"); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("unknown project: %v", err)
	}
	if err := rename(e.admin, "shop", "Not Valid"); err == nil || !strings.Contains(err.Error(), "invalid project name") {
		t.Fatalf("invalid name: %v", err)
	}

	if err := rename(e.admin, "shop", "store"); err != nil {
		t.Fatal(err)
	}
	e.api.Engine.WaitDrains()
	if _, err := e.st.Project("store"); err != nil {
		t.Fatal(err)
	}

	// The token follows the project and sees nothing under the old name.
	if err := token.Do(ctx, "GET", Path("projects", "store", "services"), nil, nil); err != nil {
		t.Fatalf("token under the new name: %v", err)
	}
	if err := token.Do(ctx, "GET", Path("projects", "shop", "services"), nil, nil); !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("token under the old name: %v", err)
	}
	// The old name is free once the rename has finished.
	if err := e.admin.Do(ctx, "POST", "/v1/projects", ProjectRequest{Name: "shop", Domains: []string{"shop.example.org"}}, nil); err != nil {
		t.Fatalf("old name after the rename: %v", err)
	}
}

// The old name stays taken while the old containers drain: they would be removed by label, along
// with anything a new project of that name had started.
func TestCreateProjectWhileItsNameDrains(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.admin.Do(ctx, "POST", "/v1/projects", ProjectRequest{Name: "store", Domains: []string{"example.com"}}, nil); err != nil {
		t.Fatal(err)
	}
	p, err := e.st.Project("store")
	if err != nil {
		t.Fatal(err)
	}
	p.RenamedFrom = "shop"
	if err := e.st.PutProject(p); err != nil {
		t.Fatal(err)
	}
	var se *StatusError
	err = e.admin.Do(ctx, "POST", "/v1/projects", ProjectRequest{Name: "shop", Domains: []string{"shop.example.org"}}, nil)
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("got %v, want 409", err)
	}
}
