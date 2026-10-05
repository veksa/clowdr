package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSelfUpdate(t *testing.T) {
	bin := []byte("new clowctl")
	sum := sha256.Sum256(bin)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v0.4.0", http.StatusFound)
	})
	mux.HandleFunc("GET /releases/download/v0.4.0/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  clowdrd_linux_amd64\n%s  clowctl_linux_amd64\n", hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("GET /releases/download/v0.4.0/clowctl_linux_amd64", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bin)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := updater{base: srv.URL + "/releases", client: srv.Client(), goos: "linux", goarch: "amd64"}
	exe := filepath.Join(t.TempDir(), "clowctl")
	os.WriteFile(exe, []byte("old"), 0o755)

	if _, err := u.update(context.Background(), exe, "0.4.0", ""); !errors.Is(err, errUpToDate) {
		t.Fatal("expected up to date, got", err)
	}
	v, err := u.update(context.Background(), exe, "v0.3.0", "")
	if err != nil || v != "v0.4.0" {
		t.Fatal(v, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(bin) {
		t.Fatalf("binary not replaced: %q", b)
	}

	// A wrong checksum must leave the binary alone.
	bin = []byte("tampered")
	os.WriteFile(exe, []byte("old"), 0o755)
	if _, err := u.update(context.Background(), exe, "v0.3.0", "v0.4.0"); err == nil {
		t.Fatal("expected checksum error")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("binary changed: %q", b)
	}

	u.goarch = "riscv64"
	if _, err := u.update(context.Background(), exe, "v0.3.0", "v0.4.0"); err == nil {
		t.Fatal("expected missing asset error")
	}
}
