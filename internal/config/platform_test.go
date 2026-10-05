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

func FuzzParsePlatform(f *testing.F) {
	f.Add([]byte(DefaultPlatform))
	f.Add([]byte("domain: example.com\nprojects:\n  shop: { domains: [example.com], memory: 300m }\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParsePlatform(b)
		if err != nil {
			return
		}
		p.DeployHost()
		for name := range p.Addons {
			p.AddonAllowed(name, "shop")
		}
	})
}

func FuzzUnits(f *testing.F) {
	for _, s := range []string{"300m", "1g", "512k", "1.5G", "24h", "14d", "30s", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if n, err := ParseBytes(s); err == nil && n >= 0 {
			if m, err := ParseBytes(FormatBytes(n)); err != nil || (n > 0 && m <= 0) {
				t.Fatalf("%q → %d → %q → %d, %v", s, n, FormatBytes(n), m, err)
			}
		}
		ParseDuration(s)
	})
}
