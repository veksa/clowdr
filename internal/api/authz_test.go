package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/veksa/clowdr/internal/spec"
)

// routes is every API route, as registered in Handler and registerOps, with whether it is
// admin-only. TestRoutesCovered keeps the list complete.
var routes = []struct {
	pattern string
	admin   bool
}{
	{"GET /v1/version", false},
	{"GET /v1/projects", true},
	{"POST /v1/projects", true},
	{"DELETE /v1/projects/{project}", true},
	{"GET /v1/tokens", true},
	{"POST /v1/tokens", true},
	{"DELETE /v1/tokens/{id}", true},
	{"GET /v1/platform/status", true},
	{"POST /v1/platform/reconcile", true},
	{"POST /v1/gc", true},
	{"GET /v1/projects/{project}", false},
	{"GET /v1/projects/{project}/services", false},
	{"POST /v1/projects/{project}/apply", false},
	{"GET /v1/projects/{project}/routes", false},
	{"GET /v1/projects/{project}/services/{service}", false},
	{"DELETE /v1/projects/{project}/services/{service}", false},
	{"POST /v1/projects/{project}/services/{service}/deploy", false},
	{"POST /v1/projects/{project}/services/{service}/rollback", false},
	{"POST /v1/projects/{project}/services/{service}/restart", false},
	{"POST /v1/projects/{project}/services/{service}/stop", false},
	{"POST /v1/projects/{project}/services/{service}/start", false},
	{"GET /v1/projects/{project}/services/{service}/releases", false},
	{"GET /v1/projects/{project}/services/{service}/logs", false},
	{"GET /v1/projects/{project}/services/{service}/env", false},
	{"PATCH /v1/projects/{project}/services/{service}/env", false},
	{"GET /v1/projects/{project}/access", false},
	{"GET /v1/projects/{project}/events", false},
	{"POST /v1/projects/{project}/services/{service}/scale", false},
	{"POST /v1/projects/{project}/services/{service}/exec", false},
	{"GET /v1/projects/{project}/top", false},
	{"GET /v1/projects/{project}/volumes", false},
	{"DELETE /v1/projects/{project}/volumes/{volume}", false},
	{"GET /v1/projects/{project}/addons", false},
	{"DELETE /v1/projects/{project}/addons/{addon}", false},
	{"POST /v1/projects/{project}/addons/{addon}/exec", false},
}

func TestRoutesCovered(t *testing.T) {
	re := regexp.MustCompile(`\b(a|admin)\("([A-Z]+ /v1/[^"]+)"`)
	registered := map[string]bool{} // pattern → admin-only
	for _, f := range []string{"server.go", "ops.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			registered[m[2]] = m[1] == "admin"
		}
	}
	for _, r := range routes {
		admin, ok := registered[r.pattern]
		switch {
		case !ok:
			t.Errorf("%s is in the matrix but not registered", r.pattern)
		case admin != r.admin:
			t.Errorf("%s: admin-only is %v in the code, %v in the matrix", r.pattern, admin, r.admin)
		}
		delete(registered, r.pattern)
	}
	for p := range registered {
		t.Errorf("route %s is not in the authorization matrix", p)
	}
}

// TestAuthorization calls every route with every kind of caller. Only the HTTP status of the
// authorization decision matters here: 401 without a valid token, 403 for another project or a
// project token on an admin route, and anything else when access is allowed.
func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	shop := e.projectClient(t, "shop", "example.com")
	blog := e.projectClient(t, "blog", "example.org")
	revoked := e.projectClient(t, "shop", "example.com")
	revokedID := strings.SplitN(strings.TrimPrefix(revoked.Token, "clw_"), "_", 2)[0]
	if err := e.admin.Do(context.Background(), "DELETE", "/v1/tokens/"+revokedID, nil, nil); err != nil {
		t.Fatal(err)
	}

	callers := []struct {
		name  string
		token string
		// expect returns the required status (0 = anything but 401/403).
		expect func(admin bool) int
	}{
		{"anonymous", "", func(bool) int { return 401 }},
		{"garbage", "clw_nope_nope", func(bool) int { return 401 }},
		{"revoked", revoked.Token, func(bool) int { return 401 }},
		{"other project", blog.Token, func(bool) int { return 403 }},
		{"own project", shop.Token, func(admin bool) int {
			if admin {
				return 403
			}
			return 0
		}},
		{"admin", e.admin.Token, func(bool) int { return 0 }},
	}
	path := strings.NewReplacer("{project}", "shop", "{service}", "web", "{id}", "nope", "{volume}", "data", "{addon}", "redis")
	call := func(pattern, token string) int {
		method, p, _ := strings.Cut(pattern, " ")
		req, _ := http.NewRequest(method, e.srv.URL+path.Replace(p), strings.NewReader("{}"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Deleting the project runs last: it also revokes the project's tokens.
	const deleteProject = "DELETE /v1/projects/{project}"
	for _, r := range routes {
		if r.pattern == deleteProject {
			continue
		}
		for _, c := range callers {
			if r.pattern == "GET /v1/version" && c.name == "other project" {
				continue // not project-scoped
			}
			got, want := call(r.pattern, c.token), c.expect(r.admin)
			switch {
			case want != 0 && got != want:
				t.Errorf("%s as %s: %d, want %d", r.pattern, c.name, got, want)
			case want == 0 && (got == 401 || got == 403):
				t.Errorf("%s as %s: denied with %d", r.pattern, c.name, got)
			}
		}
	}
	for _, c := range callers[:len(callers)-1] {
		if got := call(deleteProject, c.token); got != 401 && got != 403 {
			t.Errorf("%s as %s: %d", deleteProject, c.name, got)
		}
	}
	if got := call(deleteProject, e.admin.Token); got != 204 {
		t.Fatalf("%s as admin: %d", deleteProject, got)
	}
	if got := call("GET /v1/projects/{project}/services", shop.Token); got != 401 {
		t.Errorf("the token of a deleted project still works: %d", got)
	}
}

// Errors map to statuses clients can act on; streamed operations report them in the final
// event.
func TestErrorStatuses(t *testing.T) {
	e := newEnv(t)
	shop := e.projectClient(t, "shop", "example.com")
	ctx := context.Background()
	for _, c := range []struct {
		method, path string
		body         any
		status       int
	}{
		{"GET", "/v1/projects/shop/services/nope", nil, 404},
		{"GET", "/v1/projects/shop/services/nope/releases", nil, 404},
		{"POST", "/v1/projects/shop/services/nope/stop", nil, 404},
		{"DELETE", "/v1/projects/shop/volumes/nope", nil, 404},
		{"DELETE", "/v1/tokens/nope", nil, 404},
		{"POST", "/v1/projects/shop/services/web/deploy", map[string]any{"unknown_field": 1}, 400},
		// another project's pushed image, by name or by image ID
		{"POST", "/v1/projects/shop/services/server/deploy", DeployRequest{Spec: spec.Service{Name: "server", Port: 3000}, Image: "clowdr.local/blog/server:v1"}, 403},
		{"POST", "/v1/projects/shop/services/server/deploy", DeployRequest{Spec: spec.Service{Name: "server", Port: 3000}, Image: "64d5d9"}, 403},
		{"POST", "/v1/projects", ProjectRequest{Name: "Bad Name"}, 400},
		{"POST", "/v1/projects", ProjectRequest{Name: "blog", Domains: []string{"example.com"}}, 409},
	} {
		client := shop
		if strings.HasPrefix(c.path, "/v1/tokens") || c.path == "/v1/projects" {
			client = e.admin
		}
		err := client.Do(ctx, c.method, c.path, c.body, nil)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != c.status {
			t.Errorf("%s %s: %v, want %d", c.method, c.path, err, c.status)
		}
	}
	for _, c := range []struct {
		path string
		body any
		want string
	}{
		{"/v1/projects/shop/services/nope/rollback", RollbackRequest{}, "not found"},
		{"/v1/projects/shop/services/nope/restart", nil, "not found"},
		{"/v1/projects/shop/services/nope/scale", ScaleRequest{Replicas: 2}, "not found"},
	} {
		_, err := shop.Stream(ctx, "POST", c.path, c.body, func(Event) {})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("POST %s: %v, want %q", c.path, err, c.want)
		}
	}
}
