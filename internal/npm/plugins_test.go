package npm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/store"
)

// entry is one tarball entry; link makes it a symlink.
type entry struct {
	name, body string
	mode       int64
	link       bool
}

func tarEntries(t *testing.T, es ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range es {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Mode: mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)}
		if e.link {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.body, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !e.link {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func pkgJSON(name, version string, flag string) string {
	s := `{"name":"` + name + `","version":"` + version + `"`
	if flag != "" {
		s += `,"claude-plugin":` + flag
	}
	return s + "}"
}

func manifestJSON(name, version string) string {
	return `{"name":"` + name + `","version":"` + version + `","description":"A test plugin"}`
}

// pluginTgz is a valid plugin package with a command and an executable hook script.
func pluginTgz(t *testing.T, name, version string, extra ...entry) []byte {
	t.Helper()
	_, base, _ := strings.Cut(name, "/")
	es := []entry{
		{name: "package/package.json", body: pkgJSON(name, version, "true")},
		{name: "package/.claude-plugin/plugin.json", body: manifestJSON(base, version)},
		{name: "package/commands/hello.md", body: "Say hello.\n"},
		{name: "package/hooks/run.sh", body: "#!/bin/sh\necho hi\n", mode: 0o755},
	}
	return tarEntries(t, append(es, extra...)...)
}

func (e *env) publishData(token, name, version string, data []byte, o pubOpts) resp {
	e.t.Helper()
	return e.do("PUT", "/npm/"+strings.Replace(name, "/", "%2f", 1), token, publishDoc(name, version, data, o))
}

func (e *env) publishPlugin(token, name, version string) resp {
	e.t.Helper()
	return e.publishData(token, name, version, pluginTgz(e.t, name, version), pubOpts{fields: map[string]any{"claude-plugin": true}})
}

func (e *env) marketplace(token string) Marketplace {
	e.t.Helper()
	r := e.do("GET", "/plugins/marketplace.json", token, nil)
	mustStatus(e.t, r, 200)
	var m Marketplace
	if err := json.Unmarshal(r.body, &m); err != nil {
		e.t.Fatalf("%s: %v", r.body, err)
	}
	return m
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestPluginPublishRules(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	const v = "1.0.0"
	cases := []struct {
		what    string
		name    string
		entries []entry
		want    string // "" = published
	}{
		{"valid", "@acme/ok", nil, ""},
		{"flag false with a manifest is a package", "@acme/plain", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/plain", v, "false")},
			{name: "package/.claude-plugin/plugin.json", body: manifestJSON("other", "9.9.9")},
		}, ""},
		{"no flag with a manifest is a package", "@acme/plain2", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/plain2", v, "")},
			{name: "package/.claude-plugin/plugin.json", body: "not json"},
		}, ""},
		{"flag not a boolean", "@acme/str", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/str", v, `"yes"`)},
			{name: "package/.claude-plugin/plugin.json", body: manifestJSON("str", v)},
		}, "must be true or false"},
		{"no manifest", "@acme/bare", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/bare", v, "true")},
		}, "needs .claude-plugin/plugin.json"},
		{"manifest not an object", "@acme/arr", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/arr", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: `["x"]`},
		}, "not a JSON object"},
		{"manifest without a name", "@acme/noname", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/noname", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: `{"version":"1.0.0"}`},
		}, "has no name"},
		{"manifest name differs", "@acme/named", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/named", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: manifestJSON("other", v)},
		}, "the package name without its scope"},
		{"manifest without a version", "@acme/nover", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/nover", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: `{"name":"nover"}`},
		}, "has no version"},
		{"manifest version differs", "@acme/ver", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/ver", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: manifestJSON("ver", "0.9.0")},
		}, "must be equal"},
		{"version not a string", "@acme/num", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/num", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: `{"name":"num","version":1}`},
		}, "is not a string"},
		{"manifest is a link", "@acme/mlink", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/mlink", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: "../x.json", link: true},
		}, "link or a special file"},
		{"a link elsewhere", "@acme/link", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/link", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: manifestJSON("link", v)},
			{name: "package/skills/x", body: "/etc/passwd", link: true},
		}, "skills/x is a link"},
		{"name Claude Code cannot install", "@acme/a~b", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/a~b", v, "true")},
			{name: "package/.claude-plugin/plugin.json", body: manifestJSON("a~b", v)},
		}, "may use only"},
	}
	for _, c := range cases {
		data := pluginTgz(t, c.name, v)
		if c.entries != nil {
			data = tarEntries(t, c.entries...)
		}
		r := e.publishData(pub, c.name, v, data, pubOpts{})
		if c.want == "" {
			if r.status != 201 {
				t.Errorf("%s: %d %s", c.what, r.status, r.body)
			}
			continue
		}
		if r.status != 400 || !strings.Contains(string(r.body), c.want) {
			t.Errorf("%s: %d %s, want 400 with %q", c.what, r.status, r.body, c.want)
		}
	}
	got, _ := e.st.NPMVersion("@acme/ok", v)
	if got.Plugin == nil || got.Plugin.Name != "ok" || got.Plugin.Description != "A test plugin" || len(got.Plugin.ZipSHA256) != 64 {
		t.Fatalf("plugin record: %+v", got.Plugin)
	}
	for _, n := range []string{"@acme/plain", "@acme/plain2"} {
		if got, _ := e.st.NPMVersion(n, v); got.Plugin != nil {
			t.Errorf("%s is not a plugin: %+v", n, got.Plugin)
		}
	}
}

func TestPluginZipDeterministic(t *testing.T) {
	data := pluginTgz(t, "@acme/tool", "1.0.0",
		entry{name: "package/a-last-in-tar.txt", body: "z"},
		entry{name: "package/zeros.bin", body: strings.Repeat("\x00", 1<<20)},
		entry{name: "package/empty", body: ""},
	)
	a, err := buildZip(data, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := buildZip(data, 64<<20)
	if !bytes.Equal(a, b) {
		t.Fatal("two builds of one tarball differ")
	}
	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if !f.Modified.Equal(zipModified) {
			t.Errorf("%s: modified %v", f.Name, f.Modified)
		}
		wantMode := os.FileMode(0o644)
		if f.Name == "hooks/run.sh" {
			wantMode = 0o755
		}
		if f.Mode() != wantMode {
			t.Errorf("%s: mode %v, want %v", f.Name, f.Mode(), wantMode)
		}
		if f.UncompressedSize64 > 0 && f.UncompressedSize64 > pluginMaxRatio*f.CompressedSize64 {
			t.Errorf("%s: expands %d → %d, beyond Claude Code's ratio", f.Name, f.CompressedSize64, f.UncompressedSize64)
		}
		rc, _ := f.Open()
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Errorf("%s: %v", f.Name, err)
		}
		rc.Close()
	}
	want := []string{".claude-plugin/plugin.json", "a-last-in-tar.txt", "commands/hello.md", "empty", "hooks/run.sh", "package.json", "zeros.bin"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("entries %v, want %v", names, want)
	}
	// Another tar order of the same files gives the same zip.
	es := []entry{
		{name: "package/hooks/run.sh", body: "#!/bin/sh\necho hi\n", mode: 0o755},
		{name: "package/commands/hello.md", body: "Say hello.\n"},
		{name: "package/.claude-plugin/plugin.json", body: manifestJSON("tool", "1.0.0")},
		{name: "package/package.json", body: pkgJSON("@acme/tool", "1.0.0", "true")},
	}
	c1, _ := buildZip(tarEntries(t, es...), 1<<20)
	es[0], es[3] = es[3], es[0]
	c2, _ := buildZip(tarEntries(t, es...), 1<<20)
	if !bytes.Equal(c1, c2) {
		t.Fatal("tar order changed the zip")
	}
}

func TestMarketplace(t *testing.T) {
	e := newEnv(t)
	e.h.Domain = func() string { return "example.com" }
	pub := e.token("ci", "npm:publish:@acme/*")
	other := e.token("other-ci", "npm:publish:@beta/*")
	reader := e.token("reader", "npm:read:@acme/*", "plugins:read")
	both := e.token("both", "npm:read:@acme/*", "npm:read:@beta/*", "plugins:read")
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.1.0"), 201)
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "2.0.0-beta.1"), 201)
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme%2ftool/dist-tags/latest", pub, []byte(`"1.1.0"`)), 200)
	mustStatus(t, e.publish(pub, "@acme/lib", "1.0.0"), 201) // not a plugin
	mustStatus(t, e.publishPlugin(other, "@beta/tool", "3.0.0"), 201)
	mustStatus(t, e.publishPlugin(other, "@beta/extra", "1.0.0"), 201)

	m := e.marketplace(reader)
	if m.Name != "clowdr-example-com" || m.Owner.Name != "example.com" || m.Description == "" {
		t.Fatalf("header: %+v", m)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("plugins: %+v", m.Plugins)
	}
	p := m.Plugins[0]
	if p.Name != "tool" || p.Version != "1.1.0" || p.Description != "A test plugin" || p.Source.Source != "archive" ||
		p.Source.URL != e.srv.URL+"/plugins/@acme/tool/1.1.0.zip" {
		t.Fatalf("entry: %+v", p)
	}
	r := e.do("GET", "/plugins/@acme/tool/1.1.0.zip", reader, nil)
	mustStatus(t, r, 200)
	if sha256hex(r.body) != p.Source.SHA256 {
		t.Fatalf("served zip %s, marketplace says %s", sha256hex(r.body), p.Source.SHA256)
	}
	if r.header.Get("Content-Type") != "application/zip" || !strings.Contains(r.header.Get("Cache-Control"), "private") ||
		r.header.Get("ETag") != `"`+p.Source.SHA256+`"` {
		t.Fatalf("headers: %v", r.header)
	}
	if r := e.do("GET", "/plugins/@acme/tool/1.1.0.zip", reader, nil, "If-None-Match", `"`+p.Source.SHA256+`"`); r.status != 304 {
		t.Fatalf("conditional request: %d", r.status)
	}
	// Older live versions download too; the marketplace lists only latest.
	mustStatus(t, e.do("GET", "/plugins/@acme/tool/1.0.0.zip", reader, nil), 200)
	mustStatus(t, e.do("GET", "/plugins/@acme/lib/1.0.0.zip", reader, nil), 404)

	// Two scopes with a plugin named "tool": the first package by name keeps it.
	m = e.marketplace(both)
	var names []string
	for _, p := range m.Plugins {
		names = append(names, p.Name+"="+p.Source.URL[len(e.srv.URL):])
	}
	if strings.Join(names, " ") != "extra=/plugins/@beta/extra/1.0.0.zip tool=/plugins/@acme/tool/1.1.0.zip" {
		t.Fatalf("plugins: %v", names)
	}

	// plugins.name and plugins.owner win.
	e.h.Plugins = func() config.PluginsConf { return config.PluginsConf{Name: "acme-tools", Owner: "Acme"} }
	if m := e.marketplace(reader); m.Name != "acme-tools" || m.Owner.Name != "Acme" {
		t.Fatalf("configured: %+v", m)
	}
}

func TestPluginRightsMatrix(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.0"), 201)
	tokens := map[string]string{
		"none":         "",
		"garbage":      "clwn_0123456789ab_0123456789abcdef0123456789abcdef",
		"npm only":     e.token("npm-only", "npm:read:@acme/*"),
		"plugins only": e.token("plugins-only", "plugins:read"),
		"other scope":  e.token("other", "npm:read:@other/*", "plugins:read"),
		"both":         e.token("both", "npm:read:@acme/tool", "plugins:read"),
		"publisher":    e.token("pub-plugins", "npm:publish:@acme/*", "plugins:read"),
	}
	type want struct{ list, zip, count int }
	wants := map[string]want{
		"none":         {401, 401, 0},
		"garbage":      {401, 401, 0},
		"npm only":     {404, 404, 0},
		"plugins only": {200, 404, 0},
		"other scope":  {200, 404, 0},
		"both":         {200, 200, 1},
		"publisher":    {200, 200, 1},
	}
	missing := e.do("GET", "/plugins/@acme/nope/1.0.0.zip", tokens["both"], nil)
	mustStatus(t, missing, 404)
	for what, tok := range tokens {
		e.h.limiter.reset()
		w := wants[what]
		r := e.do("GET", "/plugins/marketplace.json", tok, nil)
		if r.status != w.list {
			t.Errorf("%s: marketplace %d, want %d: %s", what, r.status, w.list, r.body)
		}
		if r.status == 200 {
			var m Marketplace
			if err := json.Unmarshal(r.body, &m); err != nil || len(m.Plugins) != w.count || m.Plugins == nil {
				t.Errorf("%s: %s", what, r.body)
			}
			if w.count == 0 && !strings.Contains(string(r.body), `"plugins": []`) {
				t.Errorf("%s: an empty marketplace must list plugins as []: %s", what, r.body)
			}
		}
		z := e.do("GET", "/plugins/@acme/tool/1.0.0.zip", tok, nil)
		if z.status != w.zip {
			t.Errorf("%s: zip %d, want %d", what, z.status, w.zip)
		}
		if z.status == 404 && !bytes.Equal(z.body, missing.body) {
			t.Errorf("%s: refused zip differs from a missing one: %s", what, z.body)
		}
		if tok != "" && strings.Contains(string(r.body)+string(z.body), tok) {
			t.Errorf("%s: a body echoes the token", what)
		}
	}
	// npm tokens without the scope cannot use npm routes to reach the zip either; and the
	// registry stays as it was for npm.
	mustStatus(t, e.do("GET", "/npm/@acme/tool/-/tool-1.0.0.tgz", tokens["npm only"], nil), 200)
}

func TestPluginFailedAuthLimited(t *testing.T) {
	e := newEnv(t)
	ok := e.token("reader", "plugins:read")
	for range failMax {
		mustStatus(t, e.do("GET", "/plugins/marketplace.json", "clwn_bad", nil), 401)
	}
	mustStatus(t, e.do("GET", "/plugins/marketplace.json", ok, nil), 429)
	mustStatus(t, e.do("GET", "/npm/-/whoami", ok, nil), 429) // one limiter for both
}

func TestPluginZipRebuiltAndRetained(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0"} {
		mustStatus(t, e.publishPlugin(pub, "@acme/tool", v), 201)
	}
	old, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
	cur, _ := e.st.NPMVersion("@acme/tool", "1.3.0")
	oldZip, _ := e.h.zipPath(old.SHA512)
	curZip, _ := e.h.zipPath(cur.SHA512)

	// A missing zip is built again, with the same digest.
	if err := os.Remove(curZip); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/plugins/@acme/tool/1.3.0.zip", pub, nil)
	mustStatus(t, r, 200)
	if sha256hex(r.body) != cur.Plugin.ZipSHA256 {
		t.Fatal("rebuilt zip has another digest")
	}
	// A digest out of date (a Go release that deflates differently) is replaced on the rebuild,
	// and the marketplace then names the new one.
	os.Remove(curZip)
	e.st.UpdateNPM("@acme/tool", func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		for _, v := range vs {
			if v.Version == "1.3.0" {
				v.Plugin.ZipSHA256 = strings.Repeat("0", 64)
				return []store.NPMVersion{v}, nil
			}
		}
		return nil, nil
	})
	m := e.marketplace(pub)
	z := e.do("GET", "/plugins/@acme/tool/1.3.0.zip", pub, nil)
	if m.Plugins[0].Source.SHA256 != sha256hex(z.body) {
		t.Fatalf("marketplace %s, zip %s", m.Plugins[0].Source.SHA256, sha256hex(z.body))
	}

	// Retention deletes an old version's zip with its tarball.
	e.advance(14*24*time.Hour + time.Minute)
	e.h.Sweep(t.Context())
	if _, err := os.Stat(oldZip); !os.IsNotExist(err) {
		t.Fatalf("zip of a removed version kept: %v", err)
	}
	if _, err := os.Stat(curZip); err != nil {
		t.Fatalf("zip of a kept version: %v", err)
	}
	mustStatus(t, e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil), 404)

	// A zip no live version uses (an older server's retention removed the version) goes too.
	orphan := strings.Repeat("ab", 64)
	op, _ := e.h.zipPath(orphan)
	os.WriteFile(op, []byte("zip"), 0o600)
	old2 := time.Now().Add(-2 * time.Hour)
	os.Chtimes(op, old2, old2)
	e.h.Sweep(t.Context())
	if _, err := os.Stat(op); !os.IsNotExist(err) {
		t.Fatalf("orphan zip kept: %v", err)
	}
	if _, err := os.Stat(curZip); err != nil {
		t.Fatalf("a live zip was swept: %v", err)
	}
}

func TestPluginPublishedBeforePlugins(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.0"), 201)
	// As a 0.4 server stored it: no plugin record, no zip.
	v, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
	zp, _ := e.h.zipPath(v.SHA512)
	os.Remove(zp)
	e.st.UpdateNPM("@acme/tool", func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		vs[0].Plugin = nil
		return vs, nil
	})
	m := e.marketplace(pub)
	if len(m.Plugins) != 1 || m.Plugins[0].Name != "tool" {
		t.Fatalf("%+v", m)
	}
	r := e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil)
	mustStatus(t, r, 200)
	if sha256hex(r.body) != m.Plugins[0].Source.SHA256 {
		t.Fatal("digest")
	}
}

func TestPluginAudit(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.0"), 201)
	e.marketplace(pub)
	mustStatus(t, e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil), 200)
	es, err := e.h.Audit(AuditFilter{Action: "plugin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Action != "plugin.download" || es[0].Package != "@acme/tool" || es[0].Version != "1.0.0" ||
		es[1].Action != "plugin.list" || es[1].Detail != "1 plugins" || es[1].Token != "ci" {
		t.Fatalf("%+v", es)
	}
}

func TestMarketplaceName(t *testing.T) {
	for _, c := range []struct {
		conf         config.PluginsConf
		domain, host string
		want         string
	}{
		{config.PluginsConf{}, "example.com", "deploy.example.com", "clowdr-example-com"},
		{config.PluginsConf{}, "Sub.Example.co.uk", "", "clowdr-sub-example-co-uk"},
		{config.PluginsConf{}, "", "203.0.113.10:7443", "clowdr-203-0-113-10"},
		{config.PluginsConf{}, "", "[2001:db8::1]:7443", "clowdr-2001-db8-1"},
		{config.PluginsConf{Name: "acme"}, "example.com", "", "acme"},
	} {
		got := MarketplaceName(c.conf, c.domain, c.host)
		if got != c.want {
			t.Errorf("%+v: %s, want %s", c, got, c.want)
		}
		if err := config.ValidMarketplaceName(got); err != nil {
			t.Errorf("%s: %v", got, err)
		}
	}
}
