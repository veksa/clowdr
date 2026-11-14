package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/apitest"
	"github.com/veksa/clowdr/internal/logs"
)

// harness runs clowctl commands in a project directory against an in-process clowdrd.
type harness struct {
	t   *testing.T
	srv *apitest.Server
	dir string
}

const shopDeploy = `
project: shop
envs:
  prod: { context: test }
services:
  web:
    image: example/web:1
    port: 8080
    health: { path: /health, interval: 1ms, timeout: 1s }
    memory: 32m
    drain: 1ms
    domains:
      prod: [shop.example.com]
`

// setHome points every home lookup at dir: HOME on unix, USERPROFILE on Windows (HOMEDRIVE and
// HOMEPATH cleared), and fails the test when the CLI would still resolve another home, so no
// test can write a developer's real ~/.npmrc or ~/.yarnrc.yml.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	if got, err := homeDir(); err != nil || got != dir {
		t.Fatalf("home is %q (%v), not the test's %q", got, err, dir)
	}
}

// checkMode asserts a unix file mode; Windows has none to check.
func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s: mode %v, want %v", path, fi.Mode().Perm(), want)
	}
}

// needSymlinks skips a test where this system cannot create symlinks (Windows without the
// privilege or developer mode).
func needSymlinks(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	if err := os.Symlink(filepath.Join(d, "target"), filepath.Join(d, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := apitest.New(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.yml"), []byte(shopDeploy), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOWDR_CONFIG", filepath.Join(dir, "userconfig"))
	t.Setenv("CLOWDR_TOKEN", "")
	t.Setenv("CLOWDER_TOKEN", "")
	setHome(t, filepath.Join(dir, "home")) // ~/.yarnrc.yml and ~/.npmrc of npm login
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(dir)
	h := &harness{t: t, srv: srv, dir: dir}
	h.ok("context", "add", "test", srv.URL, "--token", srv.AdminToken)
	return h
}

// run executes one clowctl command line and returns its stdout and stderr.
func (h *harness) run(args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	root := newRoot(&app{version: "test", out: &out, err: &errb})
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errb)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := root.ExecuteContext(ctx)
	if d := time.Since(start); d > time.Second {
		h.t.Logf("SLOW %v: %v", args, d)
	}
	return out.String(), errb.String(), err
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	out, errOut, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("clowctl %s: %v\n%s%s", strings.Join(args, " "), err, out, errOut)
	}
	return out + errOut
}

func (h *harness) fails(args ...string) string {
	h.t.Helper()
	out, errOut, err := h.run(args...)
	if err == nil {
		h.t.Fatalf("clowctl %s: expected an error\n%s%s", strings.Join(args, " "), out, errOut)
	}
	return err.Error()
}

func contains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("missing %q in:\n%s", sub, s)
		}
	}
}

func TestProjectLifeCycle(t *testing.T) {
	h := newHarness(t)

	// Admin: project and token.
	h.ok("project", "create", "shop", "--domains", "shop.example.com", "--memory", "256m")
	contains(t, h.ok("project", "ls"), "shop", "shop.example.com", "256m")
	token := strings.TrimSpace(h.ok("token", "create", "--project", "shop", "--env", "prod", "--name", "ci"))
	if !strings.HasPrefix(token, "clw_") {
		t.Fatalf("token %q", token)
	}
	contains(t, h.ok("token", "ls"), "shop", "ci")
	contains(t, h.ok("version"), "test")
	contains(t, h.ok("platform", "status"), "nginx")

	// The project works with its own token from here on.
	t.Setenv("CLOWDR_TOKEN", token)
	h.fails("project", "create", "other", "--domains", "other.example.com")

	// diff exits with 1 when there are differences, like diff(1).
	out, errOut, err := h.run("diff")
	if code, isExit := err.(exitError); !isExit || code != 1 {
		t.Fatalf("diff before the first deploy: %v", err)
	}
	contains(t, out+errOut, "web: not deployed yet")
	contains(t, h.ok("deploy"), "web r1 live")
	h.ok("diff")
	contains(t, h.ok("ps"), "web")
	contains(t, h.ok("status", "web"), "shop.example.com")
	contains(t, h.ok("routes"), "shop.example.com")
	contains(t, h.ok("deploy", "web", "--image", "example/web:2"), "web r2 live")

	var rels []map[string]any
	if err := json.Unmarshal([]byte(h.ok("releases", "web", "-o", "json")), &rels); err != nil || len(rels) != 2 {
		t.Fatalf("releases: %v %v", rels, err)
	}
	contains(t, h.ok("releases", "web"), "example/web:2", "example/web:1")
	contains(t, h.ok("rollback", "web"), "rolled back", "example/web:1")
	contains(t, h.fails("rollback", "web", "--to", "abc"), "release id")

	h.ok("scale", "web=2")
	var st struct {
		Containers []any `json:"containers"`
	}
	if err := json.Unmarshal([]byte(h.ok("status", "web", "-o", "json")), &st); err != nil || len(st.Containers) != 2 {
		t.Fatalf("status after scale: %+v %v", st, err)
	}
	contains(t, h.fails("scale", "web"), "service=N")
	h.ok("scale", "web=1")

	h.ok("env", "set", "web", "API_KEY=secret", "MODE=prod")
	contains(t, h.ok("env", "ls", "web"), "API_KEY", "MODE")
	h.ok("env", "rm", "web", "MODE")
	if strings.Contains(h.ok("env", "ls", "web"), "MODE") {
		t.Fatal("MODE was not removed")
	}

	// A restart is recorded under the git author, as a deploy is, not the token's name.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.email")
	t.Setenv("GIT_CONFIG_VALUE_0", "dev@example.com")
	h.ok("restart", "web")
	rels = nil
	if err := json.Unmarshal([]byte(h.ok("releases", "web", "-o", "json")), &rels); err != nil || len(rels) == 0 || rels[0]["author"] != "dev@example.com" {
		t.Fatalf("restart release author: %+v %v", rels, err)
	}
	h.ok("stop", "web")
	h.ok("start", "web")
	h.ok("apply")
	contains(t, h.ok("events"), "deploy")
	contains(t, h.ok("top", "--once"), "web")
	h.ok("volume", "ls")
	h.ok("addon", "ls")

	// Logs come from the clowdr log store.
	key := logs.Key{Project: "shop", Kind: logs.KindApp, Name: "web"}
	h.srv.Logs.Append(key, logs.Line{T: time.Now().UnixNano(), R: 1, I: 1, L: "GET /health 200"})
	h.srv.Logs.Append(key, logs.Line{T: time.Now().UnixNano(), R: 1, I: 1, E: true, L: "ERROR boom"})
	h.srv.Logs.Flush(key)
	contains(t, h.ok("logs", "web"), "GET /health", "ERROR boom")
	out = h.ok("logs", "web", "--grep", "ERROR")
	if strings.Contains(out, "GET /health") {
		t.Fatalf("grep did not filter:\n%s", out)
	}
	contains(t, h.ok("logs", "--all"), "[web#1] ERROR boom")
	h.ok("access", "shop.example.com")

	h.ok("rm", "web", "--yes")
	if strings.Contains(h.ok("ps"), "web") {
		t.Fatal("web is still listed after rm")
	}

	t.Setenv("CLOWDR_TOKEN", "")
	h.ok("gc")
	h.ok("token", "revoke", tokenID(t, h))
	contains(t, h.ok("project", "rename", "shop", "store", "--yes"), "project shop is now store", "change project: in deploy.yml")
	contains(t, h.fails("project", "rename", "shop", "other", "--yes"), "not found")
	contains(t, h.ok("project", "ls"), "store")
	h.ok("project", "rm", "store", "--yes")
}

func tokenID(t *testing.T, h *harness) string {
	t.Helper()
	var ts []struct {
		ID      string `json:"id"`
		Project string `json:"project"`
	}
	if err := json.Unmarshal([]byte(h.ok("token", "ls", "-o", "json")), &ts); err != nil {
		t.Fatal(err)
	}
	for _, tk := range ts {
		if tk.Project == "shop" {
			return tk.ID
		}
	}
	t.Fatal("no shop token")
	return ""
}

func TestContexts(t *testing.T) {
	h := newHarness(t)
	contains(t, h.ok("context", "ls"), "* ", "test")
	h.fails("context", "add", "x", h.srv.URL)                         // no token
	h.fails("context", "add", "x", h.srv.URL, "--token", "clw_wrong") // rejected by the server
	h.fails("context", "use", "missing")
	h.ok("context", "add", "second", h.srv.URL, "--token", h.srv.AdminToken)
	h.ok("context", "use", "second")
	h.ok("context", "rm", "test")
	out := h.ok("context", "ls")
	if strings.Contains(out, "test") || !strings.Contains(out, "second") {
		t.Fatal(out)
	}
}

func TestOutsideProject(t *testing.T) {
	h := newHarness(t)
	os.Chdir(t.TempDir())
	contains(t, h.fails("deploy"), "deploy.yml not found")
	contains(t, h.fails("ps"), "no project")
	h.ok("project", "ls")
}

func TestDeployBuildsAndPushes(t *testing.T) {
	h := newHarness(t)
	calls := installFakeDocker(t)
	os.MkdirAll(filepath.Join(h.dir, "web"), 0o755)
	os.WriteFile(filepath.Join(h.dir, "deploy.yml"), []byte(`
project: shop
envs:
  prod: { context: test }
services:
  web:
    path: web
    build: echo built > out.txt
    build_args: { MODE: "${ENV}" }
    port: 8080
    health: { path: /health, interval: 1ms, timeout: 1s }
    drain: 1ms
    domains: { prod: [shop.example.com] }
`), 0o644)
	t.Setenv("CLOWDR_TOKEN", h.srv.Project(t, "shop", "shop.example.com"))

	contains(t, h.ok("deploy", "web", "--tag", "v1"), "web r1 live")
	if _, err := os.Stat(filepath.Join(h.dir, "web", "out.txt")); err != nil {
		t.Fatal("the build command did not run in the service directory")
	}
	host := strings.TrimPrefix(h.srv.URL, "http://")
	ref := host + "/shop/web:v1"
	contains(t, dockerCalls(t, calls), "build --platform linux/amd64", "-t "+ref, "--build-arg MODE=prod", "push "+ref)
	contains(t, h.ok("releases", "web"), "v1")

	// --no-build pushes what is already built; a missing image is an error.
	contains(t, h.ok("deploy", "web", "--tag", "v1", "--no-build"), "web r2 live")
	contains(t, h.fails("deploy", "web", "--tag", "v9", "--no-build"), "not built locally")
	contains(t, h.fails("deploy", "web", "api", "--image", "example/web:1"), "exactly one service")

	// The legacy builder would ignore --platform: no build without buildx, a built image deploys.
	t.Setenv("FAKE_DOCKER_NO_BUILDX", "1")
	contains(t, h.fails("deploy", "web", "--tag", "v1"), "docker buildx is missing")
	h.ok("deploy", "web", "--tag", "v1", "--no-build")
	t.Setenv("FAKE_DOCKER_NO_BUILDX", "")

	// A deploy keeps the deployed image and the one before it, as the server does.
	h.ok("deploy", "web", "--tag", "v2")
	contains(t, h.ok("deploy", "web", "--tag", "v3"), "web: removed 1 old local images")
	if out := h.ok("deploy", "web", "--tag", "v2", "--no-build"); strings.Contains(out, "removed") {
		t.Fatalf("redeploying the previous tag removed something:\n%s", out)
	}
	images := func() string { b, _ := os.ReadFile(filepath.Join(calls, "images")); return string(b) }
	if got := images(); strings.Contains(got, ":v1") || !strings.Contains(got, ":v2") || !strings.Contains(got, ":v3") {
		t.Fatalf("local images after deploys:\n%s", got)
	}
	contains(t, dockerCalls(t, calls), "builder prune -f --keep-storage 20GB")

	// --no-gc leaves them; gc --local then trims to the two newest.
	h.ok("deploy", "web", "--tag", "v4", "--no-gc")
	h.ok("deploy", "web", "--tag", "v5", "--no-gc")
	if got := images(); !strings.Contains(got, ":v2") || !strings.Contains(got, ":v5") {
		t.Fatalf("--no-gc removed images:\n%s", got)
	}
	h.ok("gc", "--local")
	if got := images(); strings.Contains(got, ":v3") || !strings.Contains(got, ":v4") || !strings.Contains(got, ":v5") {
		t.Fatalf("local images after gc --local:\n%s", got)
	}
}

func TestExecAndAccess(t *testing.T) {
	h := newHarness(t)
	t.Setenv("CLOWDR_TOKEN", h.srv.Project(t, "shop", "shop.example.com"))
	h.ok("deploy")
	h.ok("exec", "web", "--", "ls", "/")
	contains(t, h.fails("exec", "web"), "arg")

	key := logs.Key{Project: "shop", Kind: logs.KindAccess, Name: "_"}
	now := time.Now().UnixNano()
	for i, status := range []int{200, 200, 404, 502} {
		line := fmt.Sprintf(`{"host":"shop.example.com","status":%d,"method":"GET","uri":"/p%d","remote_addr":"203.0.113.%d","request_time":0.01}`, status, i, i)
		h.srv.Logs.Append(key, logs.Line{T: now + int64(i), L: line})
	}
	h.srv.Logs.Flush(key)
	contains(t, h.ok("access", "shop.example.com"), "/p0", "/p3")
	out := h.ok("access", "shop.example.com", "--status", "5xx")
	if strings.Contains(out, "/p0") || !strings.Contains(out, "/p3") {
		t.Fatalf("--status 5xx:\n%s", out)
	}
	contains(t, h.ok("access", "shop.example.com", "--top", "statuses"), "200", "502")
	// Shares are of all requests; the rows cut by --limit are summed up as (other).
	contains(t, h.ok("access", "shop.example.com", "--top", "paths", "--limit", "2"), "25.0%", "50.0%  (other)")
	contains(t, h.ok("access", "shop.example.com", "--export", "combined"), `"GET /p1`)
}

func TestContextPinsSelfSignedCertificate(t *testing.T) {
	h := newHarness(t)
	srv := apitest.NewTLS(t)
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock") // never a developer's colima VM

	// Without a pin the self-signed certificate is rejected.
	h.fails("context", "add", "tls", srv.URL, "--token", srv.AdminToken)
	h.fails("context", "add", "tls", srv.URL, "--token", srv.AdminToken, "--fingerprint", strings.Repeat("0", 64))

	contains(t, h.ok("context", "add", "tls", srv.URL, "--token", srv.AdminToken, "--trust-on-first-use"), "pinned certificate")
	host := strings.TrimPrefix(srv.URL, "https://")
	if _, err := os.Stat(filepath.Join(home, ".docker", "certs.d", certsDirName(host), "ca.crt")); err != nil {
		t.Fatal("docker does not trust the certificate:", err)
	}
	contains(t, h.ok("--context", "tls", "project", "ls"), "PROJECT")

	// The pinned fingerprint is what install.sh prints.
	cfg, err := loadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	fp := cfg.Contexts["tls"].Fingerprint
	if fp != api.CertFingerprint(srv.Cert) {
		t.Fatalf("pinned %s", fp)
	}
	h.ok("context", "add", "tls2", srv.URL, "--token", srv.AdminToken, "--fingerprint", fp)
}
