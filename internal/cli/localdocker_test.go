package cli

import (
	"testing"

	"github.com/veksa/clowdr/internal/config"
)

func TestLocalDockerEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"":                            true,
		"unix:///var/run/docker.sock": true,
		"npipe:////./pipe/dockerDesktopLinuxEngine": true,
		"tcp://127.0.0.1:2375":                      true,
		"tcp://localhost:2375":                      true,
		"tcp://[::1]:2375":                          true,
		"ssh://root@example.com":                    false,
		"tcp://example.com:2376":                    false,
		"tcp://203.0.113.10:2376":                   false,
	} {
		if got := localDockerEndpoint(endpoint); got != want {
			t.Errorf("localDockerEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

func TestRequireLocalDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://root@example.com")
	if err := requireLocalDocker(t.Context()); err == nil {
		t.Fatal("remote docker engine accepted")
	}
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	if err := requireLocalDocker(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestUsesDocker(t *testing.T) {
	for s, want := range map[*config.Service]bool{
		{Path: "web"}:                          true,
		{Image: "nginx:alpine"}:                false,
		{Image: "nginx:alpine", Build: "make"}: true,
		{Dockerfile: "Dockerfile.prod"}:        true,
	} {
		if got := usesDocker(*s); got != want {
			t.Errorf("usesDocker(%+v) = %v", *s, got)
		}
	}
}
