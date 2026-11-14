package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeadersHelperCommand(t *testing.T) {
	reg := "https://deploy.example.com/npm/"
	for _, c := range []struct {
		exe, goos, want string
	}{
		{"/usr/local/bin/clowctl", "linux", "/usr/local/bin/clowctl plugins headers --registry https://deploy.example.com/npm/"},
		{"/Users/a b/bin/clowctl", "darwin", "'/Users/a b/bin/clowctl' plugins headers --registry https://deploy.example.com/npm/"},
		{"/home/o'neil/clowctl", "linux", `'/home/o'\''neil/clowctl' plugins headers --registry https://deploy.example.com/npm/`},
		{`C:\tools\clowctl.exe`, "windows", `"C:/tools/clowctl.exe" plugins headers --registry https://deploy.example.com/npm/`},
		{`C:\Program Files\clowdr\clowctl.exe`, "windows", `"C:/Program Files/clowdr/clowctl.exe" plugins headers --registry https://deploy.example.com/npm/`},
		{`C:\a&b (x86)^\clowctl.exe`, "windows", `"C:/a&b (x86)^/clowctl.exe" plugins headers --registry https://deploy.example.com/npm/`},
	} {
		got, err := headersHelperCommand(c.exe, reg, c.goos)
		if err != nil || got != c.want {
			t.Errorf("%s on %s: %q %v, want %q", c.exe, c.goos, got, err, c.want)
		}
		if c.goos == "windows" && strings.Count(got, `"`) != 2 {
			t.Errorf("%s: more than two quotes for cmd.exe", got)
		}
	}
	for _, bad := range []struct{ exe, goos string }{
		{"/opt/" + strings.Repeat("x", 480) + "/clowctl", "linux"},
		{"/home/zoë/clowctl", "linux"},
		{"/home/a\nb/clowctl", "linux"},
		{`C:\a"b\clowctl.exe`, "windows"},
		{`C:\100%\clowctl.exe`, "windows"},
		{`C:\%PATH%\clowctl.exe`, "windows"},
		{`C:\wow!\clowctl.exe`, "windows"},
	} {
		if got, err := headersHelperCommand(bad.exe, reg, bad.goos); err == nil {
			t.Errorf("%q accepted: %q", bad.exe, got)
		}
	}
	if _, err := headersHelperCommand("/bin/clowctl", "https://x.example/npm/;rm -rf", "linux"); err == nil {
		t.Error("a registry URL with shell characters was accepted")
	}
}

func TestHelperPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "Cellar", "clowdr", "0.5.0", "bin", "clowctl")
	link := filepath.Join(dir, "bin", "clowctl")
	daemonLink := filepath.Join(dir, "bin", "clowdrd")
	other := filepath.Join(dir, "bin", "other")
	for _, p := range []string{real, other} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o755)
	}
	needSymlinks(t)
	os.Symlink(real, link)
	os.Symlink(real, daemonLink)
	oldExe, oldArg, oldLook, oldTemp := executable, argv0, lookPath, tempDir
	t.Cleanup(func() { executable, argv0, lookPath, tempDir = oldExe, oldArg, oldLook, oldTemp })
	tempDir = func() string { return filepath.Join(dir, "tmp") } // t.TempDir is under the real one
	executable = func() (string, error) { return real, nil }
	lookPath = func(string) (string, error) { return link, nil }

	for _, c := range []struct {
		what, arg0, want, warn string
	}{
		{"started by a link: the link stays across upgrades", link, link, ""},
		{"found on PATH", "clowctl", link, ""},
		{"started by the file itself", real, real, "Homebrew version folder"},
		{"argv[0] is another file", other, real, "Homebrew version folder"},
		{"no argv[0]", "", real, "Homebrew version folder"},
	} {
		argv0 = func() string { return c.arg0 }
		got, warn, err := helperPath()
		if err != nil || got != c.want || !strings.Contains(warn, c.warn) || (c.warn == "" && warn != "") {
			t.Errorf("%s: %q %q %v, want %q", c.what, got, warn, err, c.want)
		}
	}
	// A daemon name would start the server when Claude Code runs the helper.
	executable = func() (string, error) { return daemonLink, nil }
	argv0 = func() string { return "" }
	if _, _, err := helperPath(); err == nil || !strings.Contains(err.Error(), "clowdrd") {
		t.Errorf("clowdrd: %v", err)
	}
	// go run and go test builds live in temp folders.
	for _, p := range []string{filepath.Join(dir, "tmp", "x", "clowctl"), "/private/var/go-build123/b001/exe/clowctl"} {
		executable = func() (string, error) { return p, nil }
		if _, _, err := helperPath(); err == nil || !strings.Contains(err.Error(), "temporary build") {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestMergeMarketplace(t *testing.T) {
	const url = "https://deploy.example.com/plugins/marketplace.json"
	const helper = "/usr/local/bin/clowctl plugins headers --registry https://deploy.example.com/npm/"
	orig := `{
    "model": "opus",
    "permissions": {"allow": ["Bash(ls:*)"]},
    "extraKnownMarketplaces": {
        "team": {"source": {"source": "github", "repo": "acme/plugins"}},
        "old-name": {"source": {"source": "url", "url": "` + url + `"}},
        "clowdr-example-com": {"source": {"source": "url", "url": "https://old.example/x.json"}, "note": "<keep & me>"}
    },
    "enabledPlugins": {"x@team": true}
}`
	out, mm, err := mergeMarketplace([]byte(orig), "clowdr-example-com", url, helper)
	if err != nil {
		t.Fatal(err)
	}
	if len(mm.others) != 1 || mm.others[0] != "old-name" || mm.replaced != "the marketplace at https://old.example/x.json" || mm.manual {
		t.Fatalf("merge: %+v", mm)
	}
	s := string(out)
	// Keys stay in their places, values are kept, HTML characters are not escaped.
	order := []string{`"model"`, `"permissions"`, `"extraKnownMarketplaces"`, `"team"`, `"old-name"`, `"clowdr-example-com"`, `"enabledPlugins"`}
	last := -1
	for _, k := range order {
		i := strings.Index(s, k)
		if i <= last {
			t.Fatalf("%s out of place:\n%s", k, s)
		}
		last = i
	}
	contains(t, s, `"note": "<keep & me>"`, `"Bash(ls:*)"`, `"autoUpdate": true`, `"headersHelper": "`+helper+`"`, "\n  \"model\"")
	var parsed struct {
		Markets map[string]struct {
			Source     urlSource `json:"source"`
			AutoUpdate bool      `json:"autoUpdate"`
		} `json:"extraKnownMarketplaces"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if m := parsed.Markets["clowdr-example-com"]; m.Source != (urlSource{"url", url, helper}) || !m.AutoUpdate {
		t.Fatalf("%+v", m)
	}
	// Idempotent: the same bytes come back.
	again, _, err := mergeMarketplace(out, "clowdr-example-com", url, helper)
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("second run changed the file:\n%s", again)
	}
	// An already set up file in another formatting is left as it is.
	var compact bytes.Buffer
	json.Compact(&compact, out)
	if same, _, _ := mergeMarketplace(compact.Bytes(), "clowdr-example-com", url, helper); !bytes.Equal(same, compact.Bytes()) {
		t.Fatal("an unchanged file was reformatted")
	}
	// A missing or empty file becomes one entry.
	for _, empty := range []string{"", "  \n", "{}"} {
		out, _, err := mergeMarketplace([]byte(empty), "m", url, helper)
		if err != nil || !strings.HasPrefix(string(out), "{\n  \"extraKnownMarketplaces\": {\n    \"m\": {") {
			t.Fatalf("%q: %s %v", empty, out, err)
		}
	}
	// autoUpdate: false is the user's choice and stays; an entry that is not an object is
	// replaced, and said so.
	manual := `{"extraKnownMarketplaces": {"m": {"source": {"source": "url", "url": "` + url + `"}, "autoUpdate": false}}}`
	out, mm, err = mergeMarketplace([]byte(manual), "m", url, helper)
	if err != nil || !mm.manual || mm.replaced != "" || !strings.Contains(string(out), `"autoUpdate": false`) || strings.Contains(string(out), `"autoUpdate": true`) {
		t.Fatalf("autoUpdate false: %+v %s %v", mm, out, err)
	}
	for raw, want := range map[string]string{
		`"x"`: `not an object ("x")`,
		`{"source": {"source": "github", "repo": "acme/plugins"}}`: "the github marketplace acme/plugins",
	} {
		out, mm, err := mergeMarketplace([]byte(`{"extraKnownMarketplaces": {"m": `+raw+`}}`), "m", url, helper)
		if err != nil || mm.replaced != want || !strings.Contains(string(out), `"autoUpdate": true`) {
			t.Errorf("%s: %+v %v", raw, mm, err)
		}
	}
	for _, bad := range []string{"[]", "{", `{"a":1} {}`, `{"extraKnownMarketplaces": []}`, "// comment\n{}"} {
		if _, _, err := mergeMarketplace([]byte(bad), "m", url, helper); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSetMarketplaceFile(t *testing.T) {
	needSymlinks(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	setHome(t, home)
	settings := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o700)
	os.WriteFile(settings, []byte(`{"theme": "dark"}`), 0o600)
	changed, _, err := setMarketplace(settings, "m", "https://x.example/plugins/marketplace.json", "/bin/clowctl plugins headers --registry https://x.example/npm/")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	checkMode(t, settings, 0o600)
	if changed, _, err := setMarketplace(settings, "m", "https://x.example/plugins/marketplace.json", "/bin/clowctl plugins headers --registry https://x.example/npm/"); err != nil || changed {
		t.Fatalf("second run: %v %v", changed, err)
	}

	// Through a symlink into a git work tree: refused, the linked file untouched.
	repo := filepath.Join(home, "dotfiles")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	os.WriteFile(filepath.Join(repo, "settings.json"), []byte("{}"), 0o644)
	os.Remove(settings)
	os.Symlink(filepath.Join(repo, "settings.json"), settings)
	_, _, err = setMarketplace(settings, "m", "https://x.example/plugins/marketplace.json", "h")
	if err == nil || !strings.Contains(err.Error(), "inside the git work tree") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "settings.json")); string(b) != "{}" {
		t.Fatal("the linked file was written")
	}
	// Through a symlink to a private file: written there, the link kept.
	private := filepath.Join(home, "private", "settings.json")
	os.MkdirAll(filepath.Dir(private), 0o700)
	os.WriteFile(private, []byte("{}"), 0o640)
	os.Remove(settings)
	os.Symlink(private, settings)
	if _, _, err := setMarketplace(settings, "m", "https://x.example/plugins/marketplace.json", "h"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(settings); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	checkMode(t, private, 0o640)
	if b, _ := os.ReadFile(private); !strings.Contains(string(b), "extraKnownMarketplaces") {
		t.Fatalf("%s", b)
	}
}

// pluginTarball is a plugin package with its manifest.
func pluginTarball(t *testing.T, packageJSON, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"package/package.json", packageJSON},
		{"package/.claude-plugin/plugin.json", `{"name":"` + name + `","version":"` + version + `","description":"Says hello"}`},
		{"package/commands/hello.md", "Say hello.\n"},
	} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// The plugin commands against the real API: a plugins:read login, a published plugin, ls, setup
// (twice), the headers helper's output, and the refusals.
func TestPluginsWorkflow(t *testing.T) {
	h := newHarness(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := os.Getenv("HOME")
	reg := registryURL(h.srv.URL)
	old := packTarball
	packTarball = func(_ context.Context, dir string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return nil, err
		}
		return pluginTarball(t, string(b), "hello", "1.2.0"), nil
	}
	t.Cleanup(func() { packTarball = old })
	oldExe, oldArg := executable, argv0
	executable = func() (string, error) { return "/opt/clowdr tools/clowctl", nil }
	argv0 = func() string { return "clowctl" }
	t.Cleanup(func() { executable, argv0 = oldExe, oldArg })
	oldOS := helperOS
	helperOS = "linux"
	t.Cleanup(func() { helperOS = oldOS })

	// A login without plugins:read is told how to get one.
	h.ok("npm", "login", "--context", "test", "--name", "plain", "--scope", "@acme")
	contains(t, h.fails("plugins", "setup", "--context", "test"), "has no plugins:read", "--scope plugins:read")
	h.ok("npm", "token", "revoke", "plain", "--context", "test")
	cfg, _ := loadUserConfig()
	cfg.NPM = nil
	cfg.Save()

	h.ok("npm", "login", "--context", "test", "--name", "laptop", "--scope", "plugins:read", "--scope", "npm:publish:@acme/*")
	pkg := filepath.Join(h.dir, "plugin")
	os.MkdirAll(pkg, 0o755)
	os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@acme/hello","version":"1.2.0","claude-plugin":true,"publishConfig":{"registry":"`+reg+`"}}`), 0o644)
	contains(t, h.ok("npm", "publish", "plugin", "--context", "test"), "published @acme/hello@1.2.0")

	out, _, err := h.run("plugins", "ls", "--context", "test")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "hello@clowdr-127-0-0-1", "1.2.0", "Says hello")

	errOut := h.ok("plugins", "setup", "--context", "test")
	contains(t, errOut, "marketplace clowdr-127-0-0-1 added to", "start claude once", "/plugin marketplace update clowdr-127-0-0-1",
		"claude plugin install hello@clowdr-127-0-0-1", "claude plugin marketplace update clowdr-127-0-0-1, then claude plugin update hello@clowdr-127-0-0-1",
		"marketplace add with the URL is refused")
	settings := filepath.Join(home, ".claude", "settings.json")
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Markets map[string]struct {
			Source     urlSource `json:"source"`
			AutoUpdate bool      `json:"autoUpdate"`
		} `json:"extraKnownMarketplaces"`
	}
	json.Unmarshal(b, &s)
	m := s.Markets["clowdr-127-0-0-1"]
	if m.Source.URL != h.srv.URL+"/plugins/marketplace.json" || !m.AutoUpdate ||
		m.Source.HeadersHelper != "'/opt/clowdr tools/clowctl' plugins headers --registry "+reg {
		t.Fatalf("%s", b)
	}
	contains(t, h.ok("plugins", "setup", "--context", "test"), "already set up")

	// The helper: one JSON object on stdout, nothing on stderr (not even the expiry warning other
	// commands print), and no request: the second registry does not exist.
	cfg, _ = loadUserConfig()
	tok := cfg.npmLogin(reg).Token
	cfg.npmLogin(reg).Expires = time.Now().Add(24 * time.Hour)
	cfg.setNPMLogin(&NPMLogin{Registry: "https://deploy.example.invalid/npm/", Name: "x", Token: "clwn_x_y", Expires: time.Now().Add(time.Hour)})
	cfg.Save()
	_, warn, _ := h.run("plugins", "ls", "--context", "test")
	contains(t, warn, "expires in")
	stdout, stderr, err := h.run("plugins", "headers", "--registry", reg)
	if err != nil || stderr != "" || stdout != `{"Authorization":"Bearer `+tok+`"}`+"\n" {
		t.Fatalf("stdout %q stderr %q err %v", stdout, stderr, err)
	}
	stdout, stderr, err = h.run("plugins", "headers", "--registry", "https://deploy.example.invalid/npm")
	if err != nil || stderr != "" || stdout != `{"Authorization":"Bearer clwn_x_y"}`+"\n" {
		t.Fatalf("stdout %q stderr %q err %v", stdout, stderr, err)
	}
	stdout, _, err = h.run("plugins", "headers", "--registry", "https://other.example/npm/")
	if err == nil || stdout != "" || !strings.Contains(err.Error(), "no npm token for https://other.example/npm/") {
		t.Fatalf("missing token: %q %v", stdout, err)
	}
	contains(t, h.fails("plugins", "headers"), "--registry is required")
}

// Against a server without the marketplace the commands say so.
func TestPluginsOldServer(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/npm/-/whoami" {
			json.NewEncoder(w).Encode(map[string]string{"username": "m"})
			return
		}
		http.NotFound(w, r)
	}))
	defer old.Close()
	dir := t.TempDir()
	setHome(t, dir)
	t.Setenv("CLOWDR_CONFIG", filepath.Join(dir, "config"))
	t.Setenv("CLOWDR_TOKEN", "")
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(dir)
	h := &harness{t: t, dir: dir}
	cfg := &UserConfig{path: filepath.Join(dir, "config"), Current: "old", Contexts: map[string]*Context{"old": {Server: old.URL, Token: "clw_x_y"}},
		NPM: &NPMConfig{Logins: []*NPMLogin{{Registry: registryURL(old.URL), Name: "m", Token: "clwn_a_b", Expires: time.Now().Add(time.Hour)}}}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"plugins", "ls"}, {"plugins", "setup"}} {
		contains(t, h.fails(args...), "server too old for plugins (needs clowdrd "+pluginsMinServer)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("setup wrote settings for a server without plugins")
	}
}
