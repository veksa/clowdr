package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestLoadPlatformWritesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "etc", "platform.yml")
	plat, err := loadPlatform(p, quiet)
	if err != nil || plat.Listen != "127.0.0.1:7070" {
		t.Fatalf("%+v %v", plat, err)
	}
	if b, _ := os.ReadFile(p); string(b) != config.DefaultPlatform {
		t.Fatal("default platform.yml not written")
	}
	// An existing file is the admin's: read, never overwritten.
	os.WriteFile(p, []byte("domain: example.com\n"), 0o644)
	plat, err = loadPlatform(p, quiet)
	if err != nil || plat.Domain != "example.com" {
		t.Fatalf("%+v %v", plat, err)
	}
	os.WriteFile(p, []byte("domain: [broken"), 0o644)
	if _, err := loadPlatform(p, quiet); err == nil {
		t.Fatal("a broken platform.yml must stop the daemon")
	}
}

func TestSeedProjects(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	plat, err := config.ParsePlatform([]byte("projects:\n  shop: { domains: [example.com], memory: 300m }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seedProjects(st, plat); err != nil {
		t.Fatal(err)
	}
	p, err := st.Project("shop")
	if err != nil || !p.Seeded || p.Memory != 300<<20 || p.Domains[0] != "example.com" {
		t.Fatalf("%+v %v", p, err)
	}
	// platform.yml wins on every start.
	plat.Projects["shop"] = config.ProjectConf{Domains: []string{"example.org"}}
	seedProjects(st, plat)
	if p, _ := st.Project("shop"); p.Domains[0] != "example.org" || p.Memory != 0 {
		t.Fatalf("not updated: %+v", p)
	}
}

func TestCheckDocker(t *testing.T) {
	containerd := runtime.Info{ServerVersion: "29.1.0", DriverStatus: [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}}
	o := Options{MinDockerMajor: 29}
	if err := checkDocker(containerd, o, quiet); err != nil {
		t.Fatal(err)
	}
	old := containerd
	old.ServerVersion = "28.5.1"
	if err := checkDocker(old, o, quiet); err == nil || !strings.Contains(err.Error(), "get.docker.com") {
		t.Fatalf("old docker: %v", err)
	}
	classic := runtime.Info{ServerVersion: "29.1.0", Driver: "overlay2"}
	if err := checkDocker(classic, o, quiet); err == nil || !strings.Contains(err.Error(), "containerd-snapshotter") {
		t.Fatalf("classic store: %v", err)
	}
	// Development with --registry-dir works on any docker.
	o.RegistryDir = t.TempDir()
	if err := checkDocker(old, o, quiet); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapAdmin(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := bootstrapAdmin(st, dir, quiet); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "admin-token"))
	if err != nil || !strings.HasPrefix(string(b), "clw_") {
		t.Fatalf("admin token %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "admin-token")); os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Fatalf("admin-token mode %v", st.Mode())
	}
	// Only once: a restart does not mint another admin token.
	os.Remove(filepath.Join(dir, "admin-token"))
	if err := bootstrapAdmin(st, dir, quiet); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin-token")); !os.IsNotExist(err) {
		t.Fatal("second admin token created")
	}
	ts, _ := st.Tokens()
	if len(ts) != 1 {
		t.Fatalf("%d tokens", len(ts))
	}
}

func TestSelfSignedCert(t *testing.T) {
	dir := t.TempDir()
	c1, fp1, err := selfSignedCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	// docker accepts it from certs.d only as a CA; clients connect by IP.
	if !leaf.IsCA || len(leaf.IPAddresses) == 0 || !leaf.IPAddresses[0].IsLoopback() {
		t.Fatalf("cert: CA %v, IPs %v", leaf.IsCA, leaf.IPAddresses)
	}
	if _, fp2, _ := selfSignedCert(dir); fp2 != fp1 {
		t.Fatal("the pinned certificate changed on restart")
	}
	// Trusted as a root it verifies itself, but nothing it signs for another site.
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	signer, _ := c1.PrivateKey.(*ecdsa.PrivateKey)
	fk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ft := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	fder, err := x509.CreateCertificate(rand.Reader, ft, leaf, &fk.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	forged, _ := x509.ParseCertificate(fder)
	if _, err := forged.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.com"}); err == nil {
		t.Fatal("the self-signed CA vouches for example.com")
	}

	// Close to expiry it is replaced.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	_, fp3, err := selfSignedCert(dir)
	if err != nil || fp3 == fp1 {
		t.Fatalf("expiring certificate kept: %v", err)
	}
}

func TestFluentdLogConfig(t *testing.T) {
	c := fluentdLogConfig("shop", "web", 12, 2)
	if c.Type != "fluentd" || c.Config["fluentd-address"] != "tcp://"+ForwardAddr || c.Config["cache-disabled"] != "true" {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(c.Config["tag"], "shop") || !strings.Contains(c.Config["tag"], "web") {
		t.Fatalf("tag %q", c.Config["tag"])
	}
}

func TestPlatformConfigIsRootOnly(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("no unix permissions")
	}
	p := filepath.Join(t.TempDir(), "platform.yml")
	if _, err := loadPlatform(p, quiet); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("new platform.yml is %o, want 600", fi.Mode().Perm())
	}
	os.Chmod(p, 0o644) // written by install.sh or an older version
	if _, err := loadPlatform(p, quiet); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("existing platform.yml left at %o", fi.Mode().Perm())
	}
}
