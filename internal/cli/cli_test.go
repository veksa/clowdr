package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veksa/clowdr/internal/api"
)

func setup(t *testing.T) (dir string, a *app) {
	t.Helper()
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "deploy.yml"), []byte(`
project: shop
envs:
  dev:  { server: https://deploy.example.net }
  prod: { context: shop-prod }
services:
  server: { port: 3000 }
`), 0o644)
	os.MkdirAll(filepath.Join(dir, ".clowdr"), 0o755)
	os.WriteFile(filepath.Join(dir, ".clowdr", "token.dev"), []byte("clw_file_token\n"), 0o600)
	t.Setenv("CLOWDR_CONFIG", filepath.Join(dir, "userconfig"))
	t.Setenv("CLOWDR_TOKEN", "")
	t.Setenv("CLOWDER_TOKEN", "")
	sub := filepath.Join(dir, "server", "src")
	os.MkdirAll(sub, 0o755)
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(sub) // deploy.yml is found in a parent directory
	return dir, &app{}
}

func TestTargetResolution(t *testing.T) {
	_, a := setup(t)
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if a.env != "dev" {
		t.Fatal(a.env)
	}
	tg, err := a.target(true)
	if err != nil {
		t.Fatal(err)
	}
	if tg.client.Server != "https://deploy.example.net" || tg.client.Token != "clw_file_token" || tg.project != "shop" {
		t.Fatalf("%+v %+v", tg, tg.client)
	}
	// An explicit --context uses its own token, not the project token file.
	a.cfg.Contexts["admin"] = &Context{Server: "https://deploy.example.net", Token: "clw_admin"}
	a.contextName = "admin"
	tg, _ = a.target(true)
	if tg.client.Token != "clw_admin" {
		t.Fatal("--context token must beat the token file:", tg.client.Token)
	}
	t.Setenv("CLOWDR_TOKEN", "clw_env")
	tg, _ = a.target(true)
	if tg.client.Token != "clw_env" {
		t.Fatal("env token must win")
	}
	a.contextName = ""
	// CLOWDR_SERVER pins where the env token may go.
	t.Setenv("CLOWDR_SERVER", "https://deploy.example.net/")
	if _, err := a.target(true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOWDR_SERVER", "https://deploy.example.com")
	if _, err := a.target(true); err == nil || !strings.Contains(err.Error(), "CLOWDR_SERVER") {
		t.Fatalf("token sent to an unpinned server: %v", err)
	}
	t.Setenv("CLOWDR_SERVER", "")

	// prod uses a context that does not exist yet.
	a.env = "prod"
	if _, err := a.target(true); err == nil || !strings.Contains(err.Error(), "shop-prod") {
		t.Fatal(err)
	}
	a.cfg.Contexts["shop-prod"] = &Context{Server: "https://203.0.113.10:7443", Token: "clw_ctx", Fingerprint: "ab"}
	t.Setenv("CLOWDR_TOKEN", "")
	tg, err = a.target(true)
	if err != nil || tg.client.Server != "https://203.0.113.10:7443" || tg.client.Token != "clw_ctx" || tg.client.Fingerprint != "ab" {
		t.Fatal(tg, err)
	}
	a.env = "stage"
	if _, err := a.target(true); err == nil {
		t.Fatal("unknown env")
	}
}

func TestReplicaOf(t *testing.T) {
	if replicaOf("shop-server-r12-2") != "#2" {
		t.Fatal(replicaOf("shop-server-r12-2"))
	}
}

func TestPlatformStatusFlagsADegradedNginx(t *testing.T) {
	print := func(n api.NginxStatus) string {
		var out bytes.Buffer
		(&app{out: &out}).printPlatformStatus(api.PlatformStatus{Version: "test", Nginx: n})
		return out.String()
	}
	healthy := print(api.NginxStatus{State: "running", Image: "nginx:1.31.9-alpine", Configured: "nginx:1.31.9-alpine"})
	for _, flag := range []string{"platform.yml says", "not in the network", "has no version"} {
		if strings.Contains(healthy, flag) {
			t.Fatalf("a healthy nginx flagged %q:\n%s", flag, healthy)
		}
	}
	got := print(api.NginxStatus{State: "running", Image: "nginx:1.31.9-alpine", Configured: "nginx:alpine",
		Unpinned: true, MissingNetworks: []string{"blog", "shop"}})
	for _, flag := range []string{
		"runs nginx:1.31.9-alpine, platform.yml says nginx:alpine",
		"not in the network of blog, shop",
		"nginx.image nginx:alpine has no version",
	} {
		if !strings.Contains(got, flag) {
			t.Errorf("missing %q:\n%s", flag, got)
		}
	}
}
