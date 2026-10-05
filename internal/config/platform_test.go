package config

import "testing"

func TestDefaultPlatformParses(t *testing.T) {
	p, err := ParsePlatform([]byte(DefaultPlatform))
	if err != nil {
		t.Fatal(err)
	}
	if p.Listen != "127.0.0.1:7070" || p.Addons["redis"].Memory != 64<<20 || p.Logs.App.Max != 200<<20 {
		t.Fatalf("%+v", p)
	}
	if p.DeployHost() != "" {
		t.Fatal("no domain expected")
	}
}

func TestPlatform(t *testing.T) {
	p, err := ParsePlatform([]byte(`
domain: example.com
projects:
  shop: { domains: [example.com, "*.example.com"], addons: [redis] }
addons:
  redis: { image: redis:7-alpine, memory: 64m, allow: [shop] }
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.DeployHost() != "deploy.example.com" {
		t.Fatal(p.DeployHost())
	}
	if !p.AddonAllowed("redis", "shop") || p.AddonAllowed("redis", "blog") {
		t.Fatal("allow")
	}
	if p.Addons["redis"].Port != 6379 {
		t.Fatal("redis default port")
	}
	if _, err := ParsePlatform([]byte("projects: {a: {addons: [pg]}}")); err == nil {
		t.Fatal("unknown addon accepted")
	}
	if _, err := ParsePlatform([]byte("")); err != nil {
		t.Fatal("empty file must be valid:", err)
	}
}
