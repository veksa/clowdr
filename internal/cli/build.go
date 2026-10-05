package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// shell runs a build command with the platform shell: cmd /C on Windows, sh -c elsewhere,
// so the same deploy.yml works on every OS.
func shell(ctx context.Context, dir, command string, env []string) error {
	var cmd *exec.Cmd
	if goruntime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
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
		if out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
			if host := strings.TrimSpace(string(out)); host != "" {
				env = append(env, "DOCKER_HOST="+host)
			}
		}
	}
	return run(ctx, env, "docker", "push", ref)
}
