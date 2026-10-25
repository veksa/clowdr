package cli

import (
	"context"
	"crypto/ed25519"
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
	pub, priv, _ := ed25519.GenerateKey(nil)
	sums := fmt.Sprintf("%s  clowdrd_linux_amd64\n%s  clowctl_linux_amd64\n", hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:]))
	sig := ed25519.Sign(priv, []byte("v0.4.0\n"+sums))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v0.4.0", http.StatusFound)
	})
	mux.HandleFunc("GET /releases/download/v0.4.0/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sums)
	})
	mux.HandleFunc("GET /releases/download/v0.4.0/checksums.txt.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write(sig)
	})
	mux.HandleFunc("GET /releases/download/v0.4.0/clowctl_linux_amd64", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bin)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := updater{base: srv.URL + "/releases", client: srv.Client(), goos: "linux", goarch: "amd64", key: pub}
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

func TestSelfUpdateNeedsSignature(t *testing.T) {
	bin := []byte("evil clowctl")
	sum := sha256.Sum256(bin)
	sums := hex.EncodeToString(sum[:]) + "  clowctl_linux_amd64\n"
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	var sig []byte // nil = no signature file
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/download/v9.9.9/checksums.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sums) })
	mux.HandleFunc("GET /releases/download/v9.9.9/checksums.txt.sig", func(w http.ResponseWriter, r *http.Request) {
		if sig == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(sig)
	})
	mux.HandleFunc("GET /releases/download/v9.9.9/clowctl_linux_amd64", func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A release whose checksums match its binary is still refused unless the release key signed
	// them for this tag: an older release's files, signed for that release, do not pass.
	u := updater{base: srv.URL + "/releases", client: srv.Client(), goos: "linux", goarch: "amd64", key: pub}
	exe := filepath.Join(t.TempDir(), "clowctl")
	for _, s := range [][]byte{
		nil,
		ed25519.Sign(other, []byte("v9.9.9\n"+sums)),
		ed25519.Sign(priv, []byte("v0.1.0\n"+sums)),
		ed25519.Sign(priv, []byte(sums)),
	} {
		sig = s
		os.WriteFile(exe, []byte("old"), 0o755)
		if _, err := u.update(context.Background(), exe, "v0.3.0", "v9.9.9"); err == nil {
			t.Fatal("installed a release without a valid signature")
		}
		if b, _ := os.ReadFile(exe); string(b) != "old" {
			t.Fatalf("binary changed: %q", b)
		}
	}
}

func TestReleaseKey(t *testing.T) {
	if _, err := releasePublicKey(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOWDR_RELEASE_KEY", "nope")
	if _, err := releasePublicKey(); err == nil {
		t.Fatal("bad key accepted")
	}
}
