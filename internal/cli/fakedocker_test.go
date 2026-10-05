package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// The test binary doubles as a fake `docker` CLI: copied into PATH under that name, it records
// its arguments, "builds" by remembering the tag and pushes a random image with the credentials
// clowctl put into DOCKER_CONFIG.
func TestMain(m *testing.M) {
	if dir := os.Getenv("FAKE_DOCKER_DIR"); dir != "" && strings.HasPrefix(filepath.Base(os.Args[0]), "docker") {
		os.Exit(fakeDocker(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeDocker(dir string, args []string) int {
	f, _ := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, strings.Join(args, " "))
	f.Close()
	images := filepath.Join(dir, "images")
	known := func() []string {
		b, _ := os.ReadFile(images)
		return strings.Fields(string(b))
	}
	switch {
	case len(args) > 0 && args[0] == "build":
		i := slices.Index(args, "-t")
		f, _ := os.OpenFile(images, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, args[i+1])
		f.Close()
	case len(args) > 2 && args[0] == "image" && args[1] == "inspect":
		if !slices.Contains(known(), args[2]) {
			fmt.Fprintln(os.Stderr, "No such image:", args[2])
			return 1
		}
	case len(args) > 1 && args[0] == "image" && args[1] == "ls":
		fmt.Println(strings.Join(known(), "\n"))
	case len(args) > 1 && args[0] == "push":
		if err := fakePush(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

func fakePush(ref string) error {
	b, err := os.ReadFile(filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json"))
	if err != nil {
		return err
	}
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(cfg.Auths[r.Context().RegistryStr()].Auth)
	if err != nil {
		return err
	}
	user, pass, _ := strings.Cut(string(raw), ":")
	img, err := random.Image(128, 1)
	if err != nil {
		return err
	}
	return remote.Write(r, img, remote.WithAuth(&authn.Basic{Username: user, Password: pass}))
}

// installFakeDocker puts the fake docker first in PATH and returns the directory with its
// recorded calls.
func installFakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "docker"
	if goruntime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_DIR", dir)
	t.Setenv("DOCKER_HOST", "")
	return dir
}

func dockerCalls(t *testing.T, dir string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return string(b)
}
