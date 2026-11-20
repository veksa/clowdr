package config

import (
	"strings"
	"testing"
)

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
	if p.Nginx.Image != DefaultNginxImage {
		t.Fatalf("written default nginx image %q, want %q", p.Nginx.Image, DefaultNginxImage)
	}
	if p, _ := ParsePlatform(nil); p.Nginx.Image != DefaultNginxImage {
		t.Fatalf("default nginx image %q, want %q", p.Nginx.Image, DefaultNginxImage)
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

func TestVersionedImage(t *testing.T) {
	for ref, want := range map[string]bool{
		DefaultNginxImage:                         true,
		"nginx:1.31-alpine":                       true,
		"registry.example.test:5000/nginx:1.31.9": true,
		"nginx@sha256:0123abcd":                   true,
		"nginx":                                   false,
		"nginx:alpine":                            false,
		"nginx:latest":                            false,
		"nginx:stable-alpine":                     false,
		"registry.example.test:5000/nginx":        false, // the port is not a tag
	} {
		if got := VersionedImage(ref); got != want {
			t.Errorf("VersionedImage(%q) = %v, want %v", ref, got, want)
		}
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

func TestPlatformNPM(t *testing.T) {
	p, err := ParsePlatform([]byte(DefaultPlatform))
	if err != nil {
		t.Fatal(err)
	}
	if p.NPM.Publish || p.NPM.Keep != 3 || p.NPM.Grace.D().Hours() != 14*24 || p.NPM.MaxPublish != 64<<20 {
		t.Fatalf("defaults: %+v", p.NPM)
	}
	p, err = ParsePlatform([]byte("npm: {publish: true, keep: 5, grace: 30d, max_publish: 200m}"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.NPM.Publish || p.NPM.Keep != 5 || p.NPM.Grace.D().Hours() != 30*24 || p.NPM.MaxPublish != 200<<20 {
		t.Fatalf("%+v", p.NPM)
	}
	for _, bad := range []string{"npm: {keep: -1}", "npm: {keep: 0}", "npm: {unknown: 1}", "npm: {grace: soon}", "npm: {grace: -1d}", "npm: {grace: 0}", "npm: {grace: -5h}", "npm: {max_publish: 0}"} {
		if _, err := ParsePlatform([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPlatformPlugins(t *testing.T) {
	p, err := ParsePlatform([]byte("plugins: {name: acme-tools, owner: Acme Inc.}"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Plugins.Name != "acme-tools" || p.Plugins.Owner != "Acme Inc." {
		t.Fatalf("%+v", p.Plugins)
	}
	if p, err := ParsePlatform([]byte(DefaultPlatform)); err != nil || p.Plugins.Name != "" {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	for _, bad := range []string{
		"plugins: {name: 'acme tools'}", "plugins: {name: -acme}", "plugins: {name: a..b}", "plugins: {name: a/b}",
		"plugins: {name: claude-tools}", "plugins: {name: claudeai-x}", "plugins: {name: Anthropic-x}", "plugins: {name: inline}",
		"plugins: {name: toString}", "plugins: {name: café}", "plugins: {unknown: 1}",
		"plugins: {name: " + strings.Repeat("a", 129) + "}",
		"plugins: {max_zip: 0}", "plugins: {max_zip: 300m}",
		// a default name Claude Code would refuse
		"domain: claude-tools.example", "domain: " + strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + ".example.com",
	} {
		if _, err := ParsePlatform([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	_, err = ParsePlatform([]byte("plugins: {name: claude-x}"))
	if err == nil || !strings.Contains(err.Error(), `plugins.name "claude-x"`) || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("the error must name the key and the rule: %v", err)
	}
	_, err = ParsePlatform([]byte("domain: anthropic-fans.example"))
	if err == nil || !strings.Contains(err.Error(), "set plugins.name") {
		t.Fatalf("a refused default name must point at plugins.name: %v", err)
	}
	if p, err := ParsePlatform([]byte("domain: anthropic-fans.example\nplugins: {name: fans}")); err != nil || p.Plugins.MaxZip != DefaultPluginMaxZip {
		t.Fatalf("plugins.name fixes it: %v", err)
	}
	for _, ok := range []string{"acme", "clowdr-example-com", "Acme_Tools.v2", strings.Repeat("a", 128)} {
		if err := ValidMarketplaceName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}
