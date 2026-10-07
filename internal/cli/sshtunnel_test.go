package cli

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
)

func TestSSHTarget(t *testing.T) {
	for endpoint, want := range map[string]string{
		"ssh://example.com":           "-- example.com",
		"ssh://deploy@example.com":    "-- deploy@example.com",
		"ssh://deploy@example.com:22": "-p 22 -- deploy@example.com",
		"ssh://my-alias":              "-- my-alias",
		"ssh://example.com/":          "-- example.com",
		"unix:///var/run/docker.sock": "",
		"npipe:////./pipe/docker":     "",
		"tcp://example.com:2376":      "",
		"ssh://example.com/some/path": "", // not something docker supports either
		"":                            "",
	} {
		args, ok := sshTarget(endpoint)
		if got := strings.Join(args, " "); got != want || ok != (want != "") {
			t.Errorf("sshTarget(%q) = %q %v, want %q", endpoint, got, ok, want)
		}
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

func TestWaitDocker(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Write([]byte("OK"))
		}
	}))
	never := make(chan struct{})
	if err := waitDocker(t.Context(), "tcp://"+l.Addr().String(), never, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	close(exited)
	if err := waitDocker(t.Context(), "tcp://127.0.0.1:1", exited, 2*time.Second); err == nil || !strings.Contains(err.Error(), "ssh exited") {
		t.Fatalf("dead ssh: %v", err)
	}
}
