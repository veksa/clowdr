package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/config"
)

// versionTag returns the image tag for a service: the git commit, with a timestamp suffix
// when the working tree has uncommitted changes.
func versionTag(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "t" + time.Now().UTC().Format("20060102150405")
	}
	tag := strings.TrimSpace(string(out))
	if st, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--", ".").Output(); err == nil && len(strings.TrimSpace(string(st))) > 0 {
		tag += "-dirty-" + time.Now().UTC().Format("0102150405")
	}
	return tag
}

func gitAuthor() string {
	if out, err := exec.Command("git", "config", "user.email").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("USERNAME")
}

// shell runs a build command with sh -c, so the same deploy.yml works on every OS. On
// Windows that is the sh of Git Bash (or MSYS2/Cygwin) when clowctl runs from it or sh is on
// PATH, and cmd /C only as a last resort.
func shell(ctx context.Context, dir, command string, env []string) error {
	name, args := buildShell(goruntime.GOOS, os.Getenv, exec.LookPath)
	cmd := exec.CommandContext(ctx, name, append(args, command)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func buildShell(goos string, getenv func(string) string, lookPath func(string) (string, error)) (string, []string) {
	if goos != "windows" {
		return "sh", []string{"-c"}
	}
	if sh, err := lookPath("sh"); err == nil {
		return sh, []string{"-c"}
	}
	// Git Bash started from a shortcut puts its sh on PATH; cmd and PowerShell usually do not
	// have it, but Git for Windows installs it at a known place.
	for _, dir := range []string{getenv("ProgramFiles"), getenv("ProgramW6432"), getenv("LOCALAPPDATA") + `\Programs`} {
		if dir == "" || dir == `\Programs` {
			continue
		}
		if sh := filepath.Join(dir, "Git", "bin", "sh.exe"); fileExists(sh) {
			return sh, []string{"-c"}
		}
	}
	return "cmd", []string{"/C"}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func run(ctx context.Context, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// buildImage runs the service's build command and docker build, returning the local tag.
func (a *app) buildImage(ctx context.Context, d *config.Deploy, name, ref string) error {
	s := d.Services[name]
	dir := filepath.Join(d.Dir, filepath.FromSlash(s.Path))
	if s.Build != "" {
		cmdline := config.Expand(s.Build, a.env)
		fmt.Fprintf(a.err, "→ %s: %s\n", name, cmdline)
		if err := shell(ctx, dir, cmdline, []string{"ENV=" + a.env}); err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
	}
	dockerfile := s.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	platform := s.Platform
	if platform == "" {
		platform = "linux/amd64"
	}
	args := []string{"build", "--platform", platform, "-f", filepath.Join(dir, filepath.FromSlash(dockerfile)), "-t", ref}
	keys := make([]string, 0, len(s.BuildArgs))
	for k := range s.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+config.Expand(s.BuildArgs[k], a.env))
	}
	args = append(args, dir)
	fmt.Fprintf(a.err, "→ %s: docker %s\n", name, strings.Join(args, " "))
	if err := run(ctx, nil, "docker", args...); err != nil {
		return fmt.Errorf("docker build %s: %w", name, err)
	}
	return nil
}

// pushImage pushes with a throwaway DOCKER_CONFIG so the token never lands in
// ~/.docker/config.json. The current docker endpoint is kept via DOCKER_HOST, because the
// temporary config has no docker contexts.
func pushImage(ctx context.Context, ref, registryHost, project, token string) error {
	dir, err := os.MkdirTemp("", "clowctl-docker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	auth := base64.StdEncoding.EncodeToString([]byte(project + ":" + token))
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{registryHost: map[string]string{"auth": auth}}})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		return err
	}
	env := []string{"DOCKER_CONFIG=" + dir}
	if os.Getenv("DOCKER_HOST") == "" {
		if host := dockerEndpoint(ctx); host != "" {
			env = append(env, "DOCKER_HOST="+host)
		}
	}
	return run(ctx, env, "docker", "push", ref)
}

// usesDocker reports whether deploying s builds and pushes an image locally (anything but a
// prebuilt image the server pulls).
func usesDocker(s config.Service) bool {
	return s.Image == "" || s.Build != "" || s.Dockerfile != ""
}

// dockerEndpoint is where docker commands go: DOCKER_HOST, else the current context.
func dockerEndpoint(ctx context.Context) string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// requireLocalDocker refuses a docker engine on another machine: images are built by the
// local engine and only the new layers are pushed to the server, never built there.
func requireLocalDocker(ctx context.Context) error {
	if endpoint := dockerEndpoint(ctx); !localDockerEndpoint(endpoint) {
		return fmt.Errorf("docker points at %s: clowctl builds images with a local docker engine only (docker context use default)", endpoint)
	}
	return nil
}

func localDockerEndpoint(endpoint string) bool {
	if endpoint == "" || strings.HasPrefix(endpoint, "unix://") || strings.HasPrefix(endpoint, "npipe://") {
		return true
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "tcp" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}
