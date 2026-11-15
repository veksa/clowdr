package npm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

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

// zipOf builds the zip of a tarball and returns its bytes, checking the digest buildZip reports.
func zipOf(t *testing.T, data []byte, maxZip int64) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	b, err := buildZip(bytes.NewReader(data), dir, 64<<20, maxZip)
	if err != nil {
		if es, _ := os.ReadDir(dir); len(es) != 0 {
			t.Errorf("a failed build left %d files behind", len(es))
		}
		return nil, err
	}
	z, err := os.ReadFile(b.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if sha256hex(z) != b.sha256 || int64(len(z)) != b.size {
		t.Fatalf("buildZip says %s/%d, the file is %s/%d", b.sha256, b.size, sha256hex(z), len(z))
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("the build left %d files, want only the zip", len(es))
	}
	return z, nil
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
		// Names every system Claude Code runs on can create, once.
		{"colon", "@acme/colon", pl("@acme/colon", entry{name: "package/a:b.md", body: "x"}), "has a colon"},
		{"backslash", "@acme/bs", pl("@acme/bs", entry{name: `package\x.md`, body: "x"}), "has a backslash"},
		{"control character", "@acme/ctl", pl("@acme/ctl", entry{name: "package/a\x01b", body: "x"}), "a control character"},
		{"reserved name", "@acme/con", pl("@acme/con", entry{name: "package/commands/CON.md", body: "x"}), "Windows reserves"},
		{"reserved name in lower case", "@acme/aux", pl("@acme/aux", entry{name: "package/aux", body: "x"}), "Windows reserves"},
		{"trailing dot", "@acme/dot", pl("@acme/dot", entry{name: "package/x.", body: "x"}), "ending in a dot"},
		{"trailing space", "@acme/sp", pl("@acme/sp", entry{name: "package/x ", body: "x"}), "ending in a dot or a space"},
		{"absolute", "@acme/abs", pl("@acme/abs", entry{name: "/package/x.md", body: "x"}), "absolute path"},
		{"drive letter", "@acme/drv", pl("@acme/drv", entry{name: "C:/package/x.md", body: "x"}), "absolute path"},
		{"files differing in case", "@acme/case", pl("@acme/case",
			entry{name: "package/README.md", body: "a"}, entry{name: "package/readme.md", body: "b"}), "differ only in case"},
		{"a file and a folder differing in case", "@acme/case2", pl("@acme/case2",
			entry{name: "package/skills", body: "a"}, entry{name: "package/Skills/x.md", body: "b"}), "differ only in case"},
		{"a folder and then a file differing in case", "@acme/case3", pl("@acme/case3",
			entry{name: "package/Skills/x.md", body: "b"}, entry{name: "package/skills", body: "a"}), "differ only in case"},
		// The same names in an ordinary package are npm's business, not this server's.
		{"case clash in an ordinary package", "@acme/plain3", []entry{
			{name: "package/package.json", body: pkgJSON("@acme/plain3", v, "")},
			{name: "package/README.md", body: "a"}, {name: "package/readme.md", body: "b"},
			{name: "package/CON.md", body: "c"},
		}, ""},
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

// pl is a valid plugin package's entries plus extra.
func pl(name string, extra ...entry) []entry {
	_, base, _ := strings.Cut(name, "/")
	return append([]entry{
		{name: "package/package.json", body: pkgJSON(name, "1.0.0", "true")},
		{name: "package/.claude-plugin/plugin.json", body: manifestJSON(base, "1.0.0")},
	}, extra...)
}

// sparseTgz is a tarball whose second entry is a PAX GNU sparse 1.0 file of realsize bytes made
// of one data byte: a few hundred bytes that a tar reader expands to realsize. archive/tar does
// not write sparse files, so the PAX header is written as a regular entry and its type byte
// patched.
func sparseTgz(t *testing.T, name string, realsize int64) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	put := func(h *tar.Header, body []byte) {
		h.Size, h.Mode, h.Format = int64(len(body)), 0o644, tar.FormatUSTAR
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	pj := []byte(pkgJSON(name, "1.0.0", ""))
	put(&tar.Header{Name: "package/package.json", Typeflag: tar.TypeReg}, pj)
	paxAt := 512 + (len(pj)+511)/512*512 // tar.Writer pads an entry only when the next begins
	var recs []byte
	for _, kv := range [][2]string{
		{"GNU.sparse.major", "1"}, {"GNU.sparse.minor", "0"},
		{"GNU.sparse.name", "package/big"}, {"GNU.sparse.realsize", strconv.FormatInt(realsize, 10)},
	} {
		rec := " " + kv[0] + "=" + kv[1] + "\n"
		n := len(rec) + 1
		for len(strconv.Itoa(n))+len(rec) != n {
			n++
		}
		recs = append(recs, strconv.Itoa(n)+rec...)
	}
	put(&tar.Header{Name: "package/PaxHeaders/big", Typeflag: tar.TypeReg}, recs)
	// One region: offset 0, one byte; the map is padded to a block, then the byte follows.
	data := make([]byte, 513)
	copy(data, "1\n0\n1\n")
	data[512] = 'x'
	put(&tar.Header{Name: "package/GNUSparseFile.0/big", Typeflag: tar.TypeReg}, data)
	tw.Close()
	b := raw.Bytes()
	hdr := b[paxAt : paxAt+512]
	hdr[156] = tar.TypeXHeader
	copy(hdr[148:156], "        ")
	sum := 0
	for _, c := range hdr {
		sum += int(c)
	}
	copy(hdr[148:156], fmt.Sprintf("%06o\x00 ", sum))
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	gz.Write(b)
	gz.Close()
	return out.Bytes()
}

func TestSparseTarballRefused(t *testing.T) {
	data := sparseTgz(t, "@acme/sparse", 10<<30)
	if len(data) > 1024 {
		t.Fatalf("the sparse tarball is %d bytes", len(data))
	}
	// Go's reader does expand it: the test would prove nothing otherwise.
	tr := tar.NewReader(mustGunzip(t, data))
	tr.Next()
	if h, err := tr.Next(); err != nil || h.Size != 10<<30 {
		t.Fatalf("archive/tar reads the sparse entry as %+v, %v", h, err)
	}

	if _, err := checkTarball(data, "@acme/sparse", "1.0.0", 64<<20); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("checkTarball: %v", err)
	}
	if _, err := zipOf(t, data, 64<<20); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("buildZip: %v", err)
	}
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	r := e.publishData(pub, "@acme/sparse", "1.0.0", data, pubOpts{})
	if r.status != 400 || !strings.Contains(string(r.body), "sparse") {
		t.Fatalf("publish of an ordinary package with a sparse entry: %d %s", r.status, r.body)
	}
	// Other special entries are refused for every package too.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	pj := pkgJSON("@acme/fifo", "1.0.0", "")
	tw.WriteHeader(&tar.Header{Name: "package/package.json", Size: int64(len(pj)), Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write([]byte(pj))
	tw.WriteHeader(&tar.Header{Name: "package/pipe", Mode: 0o644, Typeflag: tar.TypeFifo})
	tw.Close()
	gz.Close()
	if r := e.publishData(pub, "@acme/fifo", "1.0.0", buf.Bytes(), pubOpts{}); r.status != 400 {
		t.Fatalf("publish with a fifo: %d %s", r.status, r.body)
	}
}

func mustGunzip(t *testing.T, data []byte) io.Reader {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

func TestPluginZipLimits(t *testing.T) {
	// A file that deflates beyond Claude Code's ratio is stored, so the zip is as large as the
	// files; plugins.max_zip refuses it.
	zeros := pluginTgz(t, "@acme/big", "1.0.0", entry{name: "package/zeros.bin", body: strings.Repeat("\x00", 2<<20)})
	z, err := zipOf(t, zeros, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	for _, f := range zr.File {
		if f.Name == "zeros.bin" && f.Method != zip.Store {
			t.Fatalf("zeros.bin method %d, want stored", f.Method)
		}
	}
	if _, err := zipOf(t, zeros, 1<<20); err == nil || !strings.Contains(err.Error(), "larger than 1048576 bytes (plugins.max_zip)") {
		t.Fatalf("over max_zip: %v", err)
	}

	e := newEnv(t)
	e.h.Plugins = func() config.PluginsConf { return config.PluginsConf{MaxZip: 1 << 20} }
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	r := e.publishData(pub, "@acme/big", "1.0.0", zeros, pubOpts{})
	if r.status != 400 || !strings.Contains(string(r.body), "plugins.max_zip") {
		t.Fatalf("publish over max_zip: %d %s", r.status, r.body)
	}
	for _, sub := range []string{"tarballs", "zips"} {
		es, _ := os.ReadDir(e.h.Dir + "/" + sub)
		if len(es) != 0 {
			t.Errorf("%s left after a refused publish: %d", sub, len(es))
		}
	}
	if _, err := e.st.NPMVersion("@acme/big", "1.0.0"); err == nil {
		t.Fatal("the refused version was stored")
	}
}

func TestPluginZipFailureCached(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only folder")
	}
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.0"), 201)
	v, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
	zp, _ := e.h.zipPath(v.SHA512)
	os.Remove(zp)
	zips := e.h.Dir + "/zips"

	// The disk fails: the zip is unavailable, the failure logged once and remembered briefly.
	os.Chmod(zips, 0o500)
	t.Cleanup(func() { os.Chmod(zips, 0o700) })
	mustStatus(t, e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil), 404)
	os.Chmod(zips, 0o700)
	mustStatus(t, e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil), 404)
	if m := e.marketplace(pub); len(m.Plugins) != 0 {
		t.Fatalf("listed with no zip: %+v", m)
	}
	if n := strings.Count(e.logs.String(), "level=ERROR msg=\"npm: plugin zip\""); n != 1 {
		t.Fatalf("the failure logged %d times, want once:\n%s", n, e.logs.String())
	}
	es, _ := e.h.Audit(AuditFilter{Action: "plugin.list"})
	if len(es) == 0 || !strings.Contains(es[0].Detail, "@acme/tool@1.0.0 (zip unavailable)") {
		t.Fatalf("audit: %+v", es)
	}
	// A minute later it is tried again.
	e.advance(zipRetrySoon + time.Second)
	r := e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil)
	mustStatus(t, r, 200)
	if cur, _ := e.st.NPMVersion("@acme/tool", "1.0.0"); sha256hex(r.body) != cur.Plugin.ZipSHA256 {
		t.Fatal("the rebuilt zip and the record disagree")
	}
	if es, _ := os.ReadDir(zips); len(es) != 1 {
		t.Fatalf("zips: %d files, want the one zip", len(es))
	}
}

// plugins.max_zip judges a publish; lowering it later does not delist what was accepted.
func TestPluginMaxZipOnlyAtPublish(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	noise := make([]byte, 64<<10)
	rand.Read(noise)
	data := pluginTgz(t, "@acme/tool", "1.0.0", entry{name: "package/noise.bin", body: string(noise)})
	mustStatus(t, e.publishData(pub, "@acme/tool", "1.0.0", data, pubOpts{}), 201)
	v, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
	zp, _ := e.h.zipPath(v.SHA512)
	os.Remove(zp)
	e.h.Plugins = func() config.PluginsConf { return config.PluginsConf{MaxZip: 16 << 10} }
	mustStatus(t, e.do("GET", "/plugins/@acme/tool/1.0.0.zip", pub, nil), 200)
	if m := e.marketplace(pub); len(m.Plugins) != 1 {
		t.Fatalf("delisted by a lower max_zip: %+v", m)
	}
	data = pluginTgz(t, "@acme/tool", "1.0.1", entry{name: "package/noise.bin", body: string(noise)})
	if r := e.publishData(pub, "@acme/tool", "1.0.1", data, pubOpts{}); r.status != 400 || !strings.Contains(string(r.body), "plugins.max_zip") {
		t.Fatalf("publish over max_zip: %d %s", r.status, r.body)
	}
}

// A version that retention removed while its zip was being rebuilt is gone, not a failure.
func TestPluginZipOfRemovedVersion(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "plugins:read")
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.0"), 201)
	v, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
	zp, _ := e.h.zipPath(v.SHA512)
	tp, _ := e.h.tarPath(v.SHA512)
	os.Remove(zp)
	os.Remove(tp)
	e.st.UpdateNPM("@acme/tool", func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		vs[0].Removed = true
		return vs, nil
	})
	if p, err := e.h.pluginOf(v); p != nil || err != nil { // v: the record as read before the removal
		t.Fatalf("%+v %v", p, err)
	}
	if strings.Contains(e.logs.String(), "level=ERROR") || len(e.h.zipFailed) != 0 {
		t.Fatalf("a removed version counted as a failure:\n%s", e.logs.String())
	}
}

// Two packages a token reads with one plugin name are both left out for that token, and only
// for it.
func TestPluginNameClashPerToken(t *testing.T) {
	e := newEnv(t)
	acme := e.token("acme", "npm:publish:@acme/*")
	beta := e.token("beta", "npm:publish:@beta/*")
	acmeOnly := e.token("acme-only", "npm:read:@acme/*", "plugins:read")
	betaOnly := e.token("beta-only", "npm:read:@beta/*", "plugins:read")
	both := e.token("both", "npm:read:@acme/*", "npm:read:@beta/*", "plugins:read")
	mustStatus(t, e.publishPlugin(acme, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.publishPlugin(beta, "@beta/tool", "1.0.0"), 201)
	mustStatus(t, e.publishPlugin(beta, "@beta/other", "1.0.0"), 201)

	for tok, want := range map[string]string{
		acmeOnly: "tool=@acme/tool",
		betaOnly: "other=@beta/other tool=@beta/tool",
		both:     "other=@beta/other",
	} {
		for range 2 {
			var got []string
			for _, p := range e.marketplace(tok).Plugins {
				got = append(got, p.Name+"="+strings.Join(strings.Split(p.Source.URL, "/")[4:6], "/"))
			}
			if strings.Join(got, " ") != want {
				t.Errorf("%s: %v, want %s", tok, got, want)
			}
		}
	}
	if n := strings.Count(e.logs.String(), "have been plugins of one name"); n != 1 {
		t.Fatalf("clash logged %d times:\n%s", n, e.logs.String())
	}
	es, _ := e.h.Audit(AuditFilter{Action: "plugin.list", Token: "both"})
	if len(es) == 0 || !strings.Contains(es[0].Detail, "plugin name tool in @acme/tool and @beta/tool") {
		t.Fatalf("audit: %+v", es)
	}
	// Both packages keep publishing.
	mustStatus(t, e.publishPlugin(acme, "@acme/tool", "1.0.1"), 201)
	mustStatus(t, e.publishPlugin(beta, "@beta/tool", "1.0.1"), 201)
	// The clashes endpoint tells the two-scope token what is hidden, and nobody else anything.
	for tok, want := range map[string]string{both: `{"clashes":[{"name":"tool","packages":["@acme/tool","@beta/tool"]}]}`, acmeOnly: `{"clashes":[]}`} {
		r := e.do("GET", "/plugins/-/clashes", tok, nil)
		if r.status != 200 || strings.TrimSpace(string(r.body)) != want {
			t.Errorf("clashes for %s: %d %s", tok, r.status, r.body)
		}
	}
	mustStatus(t, e.do("GET", "/plugins/-/clashes", e.token("npm-only", "npm:read:@acme/*"), nil), 404)
	mustStatus(t, e.do("GET", "/plugins/-/clashes", "", nil), 401)
}

// A name never changes hands: once two packages a token reads have been plugins of one name,
// nothing that happens to either of them lists the other under it for that token.
func TestPluginNameNeverChangesHands(t *testing.T) {
	setup := func(t *testing.T) (e *env, acme, beta, acmeOnly, betaOnly, both string) {
		e = newEnv(t)
		acme = e.token("acme", "npm:publish:@acme/*", "plugins:read")
		beta = e.token("beta", "npm:publish:@beta/*")
		acmeOnly = e.token("acme-only", "npm:read:@acme/*", "plugins:read")
		betaOnly = e.token("beta-only", "npm:read:@beta/*", "plugins:read")
		both = e.token("both", "npm:read:@acme/*", "npm:read:@beta/*", "plugins:read")
		mustStatus(t, e.publishPlugin(acme, "@acme/tool", "1.0.0"), 201)
		mustStatus(t, e.publishPlugin(beta, "@beta/tool", "1.0.0"), 201)
		return
	}
	listed := func(e *env, tok string) string {
		var got []string
		for _, p := range e.marketplace(tok).Plugins {
			got = append(got, p.Name+"="+strings.Join(strings.Split(p.Source.URL, "/")[4:6], "/"))
		}
		return strings.Join(got, " ")
	}

	t.Run("latest is no longer a plugin", func(t *testing.T) {
		e, acme, _, acmeOnly, betaOnly, both := setup(t)
		mustStatus(t, e.publish(acme, "@acme/tool", "2.0.0"), 201)
		if got := listed(e, both); got != "" {
			t.Fatalf("two-scope token: %q, beta took acme's name", got)
		}
		if got := listed(e, acmeOnly); got != "" {
			t.Fatalf("acme-only: %q", got)
		}
		if got := listed(e, betaOnly); got != "tool=@beta/tool" {
			t.Fatalf("beta-only: %q", got)
		}
		es, _ := e.h.Audit(AuditFilter{Action: "plugin.list", Token: "acme-only"})
		if !strings.Contains(es[0].Detail, "@acme/tool@2.0.0 (not a plugin)") {
			t.Fatalf("audit: %+v", es[0])
		}
	})
	t.Run("zip unavailable", func(t *testing.T) {
		e, _, _, acmeOnly, betaOnly, both := setup(t)
		v, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
		zp, _ := e.h.zipPath(v.SHA512)
		tp, _ := e.h.tarPath(v.SHA512)
		os.Remove(zp)
		os.Remove(tp) // the zip cannot be rebuilt
		if got := listed(e, both); got != "" {
			t.Fatalf("two-scope token: %q", got)
		}
		if got := listed(e, acmeOnly); got != "" {
			t.Fatalf("acme-only: %q", got)
		}
		if got := listed(e, betaOnly); got != "tool=@beta/tool" {
			t.Fatalf("beta-only: %q", got)
		}
	})
	t.Run("retention removes every plugin version", func(t *testing.T) {
		e, acme, _, acmeOnly, _, both := setup(t)
		for _, v := range []string{"2.0.0", "2.1.0", "2.2.0", "2.3.0"} {
			mustStatus(t, e.publish(acme, "@acme/tool", v), 201)
		}
		e.advance(15 * 24 * time.Hour)
		e.h.Sweep(t.Context())
		if v, _ := e.st.NPMVersion("@acme/tool", "1.0.0"); !v.Removed {
			t.Fatal("retention kept the plugin version: the test proves nothing")
		}
		if got := listed(e, both); got != "" {
			t.Fatalf("two-scope token: %q", got)
		}
		if got := listed(e, acmeOnly); got != "" {
			t.Fatalf("acme-only: %q", got)
		}
		claims, _ := e.st.NPMPluginClaims()
		if len(claims) != 2 {
			t.Fatalf("claims: %+v", claims)
		}
	})
	t.Run("a package that was never a plugin claims nothing", func(t *testing.T) {
		e := newEnv(t)
		acme := e.token("acme", "npm:publish:@acme/*")
		beta := e.token("beta", "npm:publish:@beta/*")
		both := e.token("both", "npm:read:@acme/*", "npm:read:@beta/*", "plugins:read")
		mustStatus(t, e.publish(acme, "@acme/tool", "1.0.0"), 201)
		forged := pubOpts{fields: map[string]any{"claude-plugin": true}}
		mustStatus(t, e.publishData(acme, "@acme/tool", "1.0.1", tgz(t, "@acme/tool", "1.0.1"), forged), 201)
		mustStatus(t, e.publishPlugin(beta, "@beta/tool", "1.0.0"), 201)
		if got := listed(e, both); got != "tool=@beta/tool" {
			t.Fatalf("two-scope token: %q", got)
		}
	})
}

// Claims are backfilled from versions stored before claims existed, and from 0.4 versions as
// they pass the check.
func TestPluginClaimsBackfill(t *testing.T) {
	e := newEnv(t)
	acme := e.token("acme", "npm:publish:@acme/*")
	beta := e.token("beta", "npm:publish:@beta/*")
	both := e.token("both", "npm:read:@acme/*", "npm:read:@beta/*", "plugins:read")
	mustStatus(t, e.publishPlugin(acme, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.publishPlugin(beta, "@beta/tool", "1.0.0"), 201)
	mustStatus(t, e.publishPlugin(acme, "@acme/old", "1.0.0"), 201)
	// @acme/old as 0.4 stored it: no plugin record, not checked.
	e.st.UpdateNPM("@acme/old", func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		vs[0].Plugin, vs[0].PluginChecked = nil, false
		return vs, nil
	})
	// A database from before claims: the bucket is gone.
	path := filepath.Join(filepath.Dir(e.h.Dir), "state.db")
	e.st.Close()
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("npm_plugin_claims")) }); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st, e.h.Store = st, st
	claimed := func() string {
		cl, _ := st.NPMPluginClaims()
		var names []string
		for _, c := range cl {
			names = append(names, c.Package+"="+c.Plugin)
		}
		return strings.Join(names, " ")
	}
	if got := claimed(); got != "@acme/tool=tool @beta/tool=tool" {
		t.Fatalf("backfilled claims: %s", got)
	}
	// tool clashes for this token; old is checked now, and claims its name.
	if m := e.marketplace(both); len(m.Plugins) != 1 || m.Plugins[0].Name != "old" {
		t.Fatalf("plugins: %+v", m.Plugins)
	}
	if got := claimed(); got != "@acme/old=old @acme/tool=tool @beta/tool=tool" {
		t.Fatalf("claims after the check: %s", got)
	}
}

// A version object that claims a plugin the tarball is not changes nothing: not for its own
// package, not for another scope's plugin of that name, not for later publishes.
func TestPluginForgedManifest(t *testing.T) {
	e := newEnv(t)
	acme := e.token("acme", "npm:publish:@acme/*")
	evil := e.token("evil", "npm:publish:@evil/*")
	acmeOnly := e.token("acme-only", "npm:read:@acme/*", "plugins:read")
	both := e.token("both", "npm:read:@acme/*", "npm:read:@evil/*", "plugins:read")
	mustStatus(t, e.publishPlugin(acme, "@acme/tool", "1.0.0"), 201)
	forged := pubOpts{fields: map[string]any{"claude-plugin": true}}
	mustStatus(t, e.publishData(evil, "@evil/tool", "1.0.0", tgz(t, "@evil/tool", "1.0.0"), forged), 201)
	got, _ := e.st.NPMVersion("@evil/tool", "1.0.0")
	if got.Plugin != nil || !got.PluginChecked || manifestSaysPlugin(got.Manifest) {
		t.Fatalf("stored: plugin %+v checked %v manifest %s", got.Plugin, got.PluginChecked, got.Manifest)
	}
	for _, tok := range []string{acmeOnly, both} {
		if m := e.marketplace(tok); len(m.Plugins) != 1 || !strings.Contains(m.Plugins[0].Source.URL, "@acme/tool") {
			t.Fatalf("%s: %+v", tok, m.Plugins)
		}
	}
	mustStatus(t, e.publishPlugin(acme, "@acme/tool", "1.0.1"), 201)
	mustStatus(t, e.do("GET", "/plugins/@evil/tool/1.0.0.zip", both, nil), 404)
	// The reverse holds as well: a plugin's stored version object says what its tarball says.
	if v, _ := e.st.NPMVersion("@acme/tool", "1.0.1"); !manifestSaysPlugin(v.Manifest) {
		t.Fatalf("plugin manifest: %s", v.Manifest)
	}
	// And the npm document serves no claim the server did not check.
	r := e.do("GET", "/npm/@evil%2ftool", both, nil)
	mustStatus(t, r, 200)
	if strings.Contains(string(r.body), "claude-plugin") {
		t.Fatalf("document: %s", r.body)
	}
}

func TestPluginZipDeterministic(t *testing.T) {
	data := pluginTgz(t, "@acme/tool", "1.0.0",
		entry{name: "package/a-last-in-tar.txt", body: "z"},
		entry{name: "package/zeros.bin", body: strings.Repeat("\x00", 1<<20)},
		entry{name: "package/empty", body: ""},
	)
	a, err := zipOf(t, data, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := zipOf(t, data, 64<<20)
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
	c1, _ := zipOf(t, tarEntries(t, es...), 1<<20)
	es[0], es[3] = es[3], es[0]
	c2, _ := zipOf(t, tarEntries(t, es...), 1<<20)
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
	// Another scope may publish a plugin of the same name: a publish is judged on its own tarball.
	mustStatus(t, e.publishPlugin(other, "@beta/tool", "3.0.0"), 201)
	mustStatus(t, e.publishPlugin(other, "@beta/extra", "1.0.0"), 201)
	mustStatus(t, e.publish(other, "@beta/lib", "1.0.0"), 201)

	m := e.marketplace(reader)
	if m.Name != "clowdr-example-com" || m.Owner.Name != "example.com" || m.Description == "" {
		t.Fatalf("header: %+v", m)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("plugins: %+v", m.Plugins)
	}
	p := m.Plugins[0]
	if p.Name != "tool" || p.Version != "1.1.0" || p.Description != "A test plugin" || p.Source.Source != "archive" ||
		p.Source.URL != "https://deploy.example.com/plugins/@acme/tool/1.1.0.zip" {
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

	// A token that reads both scopes gets neither "tool" (see TestPluginNameClashPerToken).
	m = e.marketplace(both)
	var names []string
	for _, p := range m.Plugins {
		names = append(names, p.Name+"="+strings.TrimPrefix(p.Source.URL, "https://deploy.example.com"))
	}
	if strings.Join(names, " ") != "extra=/plugins/@beta/extra/1.0.0.zip" {
		t.Fatalf("plugins: %v", names)
	}

	// The URLs name the deploy host whatever Host the request carried, and keep the port of a
	// request to the deploy host itself.
	for host, want := range map[string]string{
		"evil.example.net":        "https://deploy.example.com/plugins/",
		"evil.example.net:8443":   "https://deploy.example.com/plugins/",
		"deploy.example.com":      "https://deploy.example.com/plugins/",
		"deploy.example.com:443":  "https://deploy.example.com/plugins/",
		"deploy.example.com:8443": "https://deploy.example.com:8443/plugins/",
		"DEPLOY.example.com:8443": "https://deploy.example.com:8443/plugins/",
	} {
		r := e.do("GET", "/plugins/marketplace.json", reader, nil, "Host", host)
		var hm Marketplace
		if r.status != 200 || json.Unmarshal(r.body, &hm) != nil || len(hm.Plugins) != 1 ||
			!strings.HasPrefix(hm.Plugins[0].Source.URL, want) || strings.Contains(string(r.body), "evil") {
			t.Errorf("Host %s: %d %s, want URLs under %s", host, r.status, r.body, want)
		}
	}
	// Without a domain the links follow the request.
	e.h.Domain = nil
	if m := e.marketplace(reader); m.Plugins[0].Source.URL != e.srv.URL+"/plugins/@acme/tool/1.1.0.zip" {
		t.Fatalf("no domain: %+v", m.Plugins[0])
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
		if z.status == 404 {
			same404(t, what, z, missing)
		}
		if tok != "" && strings.Contains(string(r.body)+string(z.body), tok) {
			t.Errorf("%s: a body echoes the token", what)
		}
	}
	// A scope is not a prefix, a removed version is gone, and a version that is not a plugin
	// has no zip: all the same 404.
	acme := e.token("acme-reader", "npm:read:@acme/*", "plugins:read")
	mustStatus(t, e.publishPlugin(e.token("acmex", "npm:publish:@acmex/*"), "@acmex/x", "1.0.0"), 201)
	mustStatus(t, e.publishPlugin(pub, "@acme/tool", "1.0.1"), 201)
	mustStatus(t, e.publish(pub, "@acme/lib", "1.0.0"), 201)
	e.st.UpdateNPM("@acme/tool", func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		for _, v := range vs {
			if v.Version == "1.0.1" {
				v.Removed = true
				return []store.NPMVersion{v}, nil
			}
		}
		return nil, nil
	})
	e.h.limiter.reset()
	missing = e.do("GET", "/plugins/@acme/nope/1.0.0.zip", acme, nil)
	for _, path := range []string{"/plugins/@acmex/x/1.0.0.zip", "/plugins/@acme/tool/1.0.1.zip", "/plugins/@acme/lib/1.0.0.zip"} {
		r := e.do("GET", path, acme, nil)
		if r.status != 404 {
			t.Errorf("%s: %d", path, r.status)
		}
		same404(t, path, r, missing)
	}
	if m := e.marketplace(acme); len(m.Plugins) != 1 || m.Plugins[0].Name != "tool" || m.Plugins[0].Version != "1.0.0" {
		t.Fatalf("@acme/* reader: %+v", m.Plugins)
	}

	// npm tokens without the scope cannot use npm routes to reach the zip either; and the
	// registry stays as it was for npm.
	mustStatus(t, e.do("GET", "/npm/@acme/tool/-/tool-1.0.0.tgz", tokens["npm only"], nil), 200)
}

// same404 checks that a refused request cannot be told from a missing resource.
func same404(t *testing.T, what string, got, want resp) {
	t.Helper()
	if !bytes.Equal(got.body, want.body) {
		t.Errorf("%s: 404 body %q, a missing zip gets %q", what, got.body, want.body)
	}
	for k := range want.header {
		if k != "Date" && got.header.Get(k) != want.header.Get(k) {
			t.Errorf("%s: header %s %q, a missing zip gets %q", what, k, got.header.Get(k), want.header.Get(k))
		}
	}
	for k := range got.header {
		if _, ok := want.header[k]; !ok {
			t.Errorf("%s: header %s that a missing zip does not have", what, k)
		}
	}
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
		vs[0].Plugin, vs[0].PluginChecked = nil, false
		return vs, nil
	})
	// And one whose version object claimed a plugin its tarball is not.
	mustStatus(t, e.publish(pub, "@acme/fake", "1.0.0"), 201)
	e.st.UpdateNPM("@acme/fake", func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		vs[0].PluginChecked, vs[0].Manifest = false, json.RawMessage(`{"name":"@acme/fake","version":"1.0.0","claude-plugin":true}`)
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
	// The answers are recorded: the tarballs are read once.
	if v, _ := e.st.NPMVersion("@acme/tool", "1.0.0"); v.Plugin == nil || !v.PluginChecked {
		t.Fatalf("tool: %+v", v)
	}
	if v, _ := e.st.NPMVersion("@acme/fake", "1.0.0"); v.Plugin != nil || !v.PluginChecked {
		t.Fatalf("fake: %+v", v)
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
