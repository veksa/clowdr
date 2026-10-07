package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

// With a docker endpoint over SSH (docker context --docker host=ssh://...), every docker and
// buildx call opens its own SSH connection, and a build makes about ten of them: over a
// 70 ms link that is ~15 s per build. dockerTunnel opens one connection for the whole
// deploy instead, forwarding a local socket to the server's docker.sock, and points
// DOCKER_HOST at it.

const remoteDockerSocket = "/var/run/docker.sock"

// sshTarget turns ssh://[user@]host[:port] into ssh arguments.
func sshTarget(endpoint string) ([]string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
		return nil, false
	}
	var args []string
	if p := u.Port(); p != "" {
		args = append(args, "-p", p)
	}
	dest := u.Hostname()
	if u.User != nil && u.User.Username() != "" {
		dest = u.User.Username() + "@" + dest
	}
	return append(args, "--", dest), true
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

// dockerTunnel sets up the tunnel when docker is reached over SSH and returns a function that
// tears it down and restores the environment. Without SSH, or when the tunnel fails, docker
// keeps its own per-command SSH connections.
func (a *app) dockerTunnel(ctx context.Context) func() {
	endpoint := dockerEndpoint(ctx)
	target, ok := sshTarget(endpoint)
	if !ok {
		return func() {}
	}
	// A unix socket in a private directory where possible. Native Windows programs cannot use
	// the sockets of Git Bash's ssh, so there it is a loopback port: other local users could
	// reach the server's docker through it while the deploy runs.
	dir, err := os.MkdirTemp("", "clowctl-ssh-")
	if err != nil {
		return func() {}
	}
	var local, dockerHost string
	if goruntime.GOOS == "windows" {
		port, err := freePort()
		if err != nil {
			os.RemoveAll(dir)
			return func() {}
		}
		local = fmt.Sprintf("127.0.0.1:%d", port)
		dockerHost = "tcp://" + local
	} else {
		local = filepath.Join(dir, "docker.sock")
		dockerHost = "unix://" + local
	}
	args := append([]string{"-N", "-T", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15",
		"-L", local + ":" + remoteDockerSocket}, target...)
	cmd := exec.Command("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	fail := func(why string) func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		os.RemoveAll(dir)
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			why += ": " + msg
		}
		fmt.Fprintf(a.err, "→ docker over %s: one connection per command (%s)\n", endpoint, why)
		return func() {}
	}
	if err := cmd.Start(); err != nil {
		return fail(err.Error())
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	if err := waitDocker(ctx, dockerHost, exited, 30*time.Second); err != nil {
		return fail(err.Error())
	}

	prevHost, hadHost := os.LookupEnv("DOCKER_HOST")
	prevCtx, hadCtx := os.LookupEnv("DOCKER_CONTEXT")
	os.Setenv("DOCKER_HOST", dockerHost)
	os.Unsetenv("DOCKER_CONTEXT") // docker refuses DOCKER_HOST together with DOCKER_CONTEXT
	return func() {
		restoreEnv("DOCKER_HOST", prevHost, hadHost)
		restoreEnv("DOCKER_CONTEXT", prevCtx, hadCtx)
		cmd.Process.Kill()
		<-exited
		os.RemoveAll(dir)
	}
}

// waitDocker waits until the docker API answers /_ping through the tunnel.
func waitDocker(ctx context.Context, dockerHost string, exited <-chan struct{}, timeout time.Duration) error {
	network, addr := "tcp", strings.TrimPrefix(dockerHost, "tcp://")
	if a, ok := strings.CutPrefix(dockerHost, "unix://"); ok {
		network, addr = "unix", a
	}
	deadline := time.Now().Add(timeout)
	for {
		if ping(network, addr) == nil {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("ssh exited")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("docker did not answer through the tunnel within %s", timeout)
		}
	}
}

func ping(network, addr string) error {
	c, err := net.DialTimeout(network, addr, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, " 200 ") {
		return fmt.Errorf("docker answered %q", strings.TrimSpace(line))
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func restoreEnv(k, v string, had bool) {
	if had {
		os.Setenv(k, v)
	} else {
		os.Unsetenv(k)
	}
}
