package cli

import (
	"reflect"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/spec"
)

func TestSpecDiff(t *testing.T) {
	body := int64(10 << 20)
	old := spec.Service{Name: "api", Port: 3000, Memory: 256 << 20, Domains: []string{"shop.example.com"},
		Health: spec.Health{Path: "/health", Timeout: time.Minute}}
	new := old
	new.Memory = 512 << 20
	new.Domains = []string{"shop.example.com", "api.example.com"}
	new.Health.Path = "/ready"
	new.MaxBody = &body
	new.Routes = []spec.Route{{Path: "/admin", Service: "admin"}}

	got := specDiff(old, new)
	want := []fieldChange{
		{"health.path", "/health", "/ready"},
		{"memory", "256m", "512m"},
		{"domains", "[shop.example.com]", "[shop.example.com, api.example.com]"},
		{"routes", "-", "[{path=/admin service=admin}]"},
		{"max_body", "-", "10m"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if d := specDiff(old, old); len(d) != 0 {
		t.Fatal(d)
	}
}
