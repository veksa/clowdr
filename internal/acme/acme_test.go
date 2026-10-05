package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newManager(t *testing.T) *Manager {
	dir := t.TempDir()
	return &Manager{
		StateDir: filepath.Join(dir, "state"), CertDir: filepath.Join(dir, "certs"),
		ChallengeDir: filepath.Join(dir, "challenge"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// writeCert puts a self-signed certificate for domain, issued age ago and valid for lifetime,
// into certDir.
func writeCert(t *testing.T, certDir, domain string, age, lifetime time.Duration) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	issued := time.Now().Add(-age)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain},
		DNSNames: []string{domain}, NotBefore: issued, NotAfter: issued.Add(lifetime)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(certDir, domain), 0o755)
	os.WriteFile(filepath.Join(certDir, domain, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func TestNeedsCert(t *testing.T) {
	const day = 24 * time.Hour
	m := newManager(t)
	d := "deploy.example.com"
	if !m.NeedsCert(d) {
		t.Fatal("no certificate yet")
	}
	// Renewal starts when a third of the lifetime is left.
	for _, c := range []struct {
		age, lifetime time.Duration
		renew         bool
	}{
		{10 * day, 90 * day, false},
		{59 * day, 90 * day, false},
		{61 * day, 90 * day, true},
		{1 * day, 6 * day, false}, // short-lived certificates are not renewed on every check
		{5 * day, 6 * day, true},
		{100 * day, 90 * day, true}, // expired
	} {
		writeCert(t, m.CertDir, d, c.age, c.lifetime)
		if m.NeedsCert(d) != c.renew {
			t.Errorf("issued %v ago, valid %v: renew = %v", c.age, c.lifetime, !c.renew)
		}
	}
	m.RenewBefore = 5 * day
	writeCert(t, m.CertDir, d, 80*day, 90*day)
	if m.NeedsCert(d) {
		t.Fatal("10 days left, renewing 5 days before")
	}
	os.WriteFile(filepath.Join(m.CertDir, d, "fullchain.pem"), []byte("garbage"), 0o644)
	if _, err := CertExpiry(m.CertDir, d); err == nil || !m.NeedsCert(d) {
		t.Fatal("a broken certificate must be replaced")
	}
}

func TestFailuresBackOff(t *testing.T) {
	m := newManager(t)
	m.Directory = "http://127.0.0.1:1/dir" // nothing listens
	ctx := context.Background()
	if issued, err := m.Ensure(ctx, []string{"stream.example.com"}); err == nil || len(issued) != 0 {
		t.Fatalf("issued %v, err %v", issued, err)
	}
	// Within the hour the domain is not retried (Let's Encrypt rate limits).
	if _, err := m.Ensure(ctx, []string{"stream.example.com"}); err != nil {
		t.Fatalf("retried too early: %v", err)
	}
	m.failed["stream.example.com"] = time.Now().Add(-2 * time.Hour)
	if _, err := m.Ensure(ctx, []string{"stream.example.com"}); err == nil {
		t.Fatal("not retried after the back-off")
	}
}

func TestAccountKeyIsKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "acme-account.pem")
	k1, err := loadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := loadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Public().(*ecdsa.PublicKey).Equal(k2.Public()) {
		t.Fatal("a new account key on every start")
	}
	os.WriteFile(p, []byte("garbage"), 0o600)
	if _, err := loadOrCreateKey(p); err == nil {
		t.Fatal("a broken key must be an error, not silently replaced")
	}
}

// TestPebble gets real certificates from Pebble, Let's Encrypt's test server, in docker. Pebble
// is told to accept every challenge; the HTTP-01 token still has to be written for nginx.
func TestPebble(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (-short)")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("no docker")
	}
	name := "clowdr-pebble-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000")
	name = strings.ReplaceAll(name, ".", "")
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "PEBBLE_VA_ALWAYS_VALID=1", "-e", "PEBBLE_WFE_NONCEREJECT=0",
		"-p", "127.0.0.1::14000", "-p", "127.0.0.1::15000", "ghcr.io/letsencrypt/pebble:latest").CombinedOutput()
	if err != nil {
		t.Fatalf("start pebble: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	port := func(p string) string {
		out, err := exec.Command("docker", "port", name, p).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}
	acmeAddr, mgmtAddr := port("14000"), port("15000")

	// Pebble's own API certificate is not what we test; its issuing root is.
	insecure := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	var root []byte
	for i := 0; ; i++ {
		resp, err := insecure.Get("https://" + mgmtAddr + "/roots/0")
		if err == nil {
			root, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if i > 50 {
			t.Fatal("pebble did not start:", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	m := newManager(t)
	m.Directory, m.Email, m.HTTPClient = "https://"+acmeAddr+"/dir", "ops@example.com", insecure
	ctx := context.Background()
	domains := []string{"deploy.example.com", "stream.example.com"}
	issued, err := m.Ensure(ctx, domains)
	if err != nil || len(issued) != 2 {
		t.Fatalf("issued %v: %v", issued, err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(root) {
		t.Fatal("bad pebble root")
	}
	for _, d := range domains {
		pair, err := tls.LoadX509KeyPair(filepath.Join(m.CertDir, d, "fullchain.pem"), filepath.Join(m.CertDir, d, "privkey.pem"))
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(pair.Certificate[0])
		inter := x509.NewCertPool()
		for _, c := range pair.Certificate[1:] {
			ic, _ := x509.ParseCertificate(c)
			inter.AddCert(ic)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: d, Roots: roots, Intermediates: inter}); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if st, _ := os.Stat(filepath.Join(m.CertDir, d, "privkey.pem")); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
			t.Fatalf("private key is readable by others: %v", st.Mode())
		}
	}
	if es, _ := os.ReadDir(m.ChallengeDir); len(es) != 0 {
		t.Fatalf("challenge tokens left behind: %v", es)
	}

	// Valid certificates are not requested again; the account is reused after a restart.
	if issued, err := m.Ensure(ctx, domains); err != nil || len(issued) != 0 {
		t.Fatalf("re-issued %v: %v", issued, err)
	}
	m2 := newManager(t)
	m2.StateDir, m2.Directory, m2.HTTPClient = m.StateDir, m.Directory, insecure
	if issued, err := m2.Ensure(ctx, []string{"www.example.com"}); err != nil || len(issued) != 1 {
		t.Fatalf("after restart: %v %v", issued, err)
	}
}
