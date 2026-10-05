// Package acme obtains and renews Let's Encrypt certificates with HTTP-01 for deploy.<domain>
// and domains with `tls: direct`. Challenge files are served by clowdrd behind nginx; the
// certificates are written to CertDir, which nginx mounts read-only.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

type Manager struct {
	Directory    string // ACME directory URL; default Let's Encrypt
	Email        string
	StateDir     string // account key
	CertDir      string // <domain>/fullchain.pem, privkey.pem
	ChallengeDir string // HTTP-01 tokens
	Log          *slog.Logger
	// RenewBefore renews certificates expiring sooner than this (default 30 days).
	RenewBefore time.Duration

	mu     sync.Mutex
	client *acme.Client
	failed map[string]time.Time // back off after failures (rate limits)
}

// NeedsCert reports whether domain has no certificate or it expires soon.
func (m *Manager) NeedsCert(domain string) bool {
	exp, err := CertExpiry(m.CertDir, domain)
	if err != nil {
		return true
	}
	rb := m.RenewBefore
	if rb == 0 {
		rb = 30 * 24 * time.Hour
	}
	return time.Until(exp) < rb
}

// CertExpiry returns when the certificate of domain in certDir expires.
func CertExpiry(certDir, domain string) (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(certDir, domain, "fullchain.pem"))
	if err != nil {
		return time.Time{}, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return time.Time{}, errors.New("fullchain.pem: no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

// Ensure obtains certificates for domains that need one. It returns the domains that got a
// new certificate (nginx must reload to use them).
func (m *Manager) Ensure(ctx context.Context, domains []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var issued []string
	var errs []error
	for _, d := range domains {
		if !m.NeedsCert(d) {
			continue
		}
		if t, ok := m.failed[d]; ok && time.Since(t) < time.Hour {
			continue
		}
		m.Log.Info("acme: requesting certificate", "domain", d)
		if err := m.obtain(ctx, d); err != nil {
			if m.failed == nil {
				m.failed = map[string]time.Time{}
			}
			m.failed[d] = time.Now()
			errs = append(errs, fmt.Errorf("%s: %w", d, err))
			continue
		}
		delete(m.failed, d)
		issued = append(issued, d)
		m.Log.Info("acme: certificate installed", "domain", d)
	}
	return issued, errors.Join(errs...)
}

func (m *Manager) acmeClient(ctx context.Context) (*acme.Client, error) {
	if m.client != nil {
		return m.client, nil
	}
	key, err := loadOrCreateKey(filepath.Join(m.StateDir, "acme-account.pem"))
	if err != nil {
		return nil, err
	}
	dir := m.Directory
	if dir == "" {
		dir = acme.LetsEncryptURL
	}
	c := &acme.Client{Key: key, DirectoryURL: dir, UserAgent: "clowdr"}
	acct := &acme.Account{}
	if m.Email != "" {
		acct.Contact = []string{"mailto:" + m.Email}
	}
	if _, err := c.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("acme register: %w", err)
	}
	m.client = c
	return c, nil
}

func (m *Manager) obtain(ctx context.Context, domain string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	c, err := m.acmeClient(ctx)
	if err != nil {
		return err
	}
	order, err := c.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return err
	}
	for _, u := range order.AuthzURLs {
		z, err := c.GetAuthorization(ctx, u)
		if err != nil {
			return err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, ch := range z.Challenges {
			if ch.Type == "http-01" {
				chal = ch
			}
		}
		if chal == nil {
			return errors.New("no http-01 challenge offered")
		}
		body, err := c.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(m.ChallengeDir, 0o755); err != nil {
			return err
		}
		tokenPath := filepath.Join(m.ChallengeDir, chal.Token)
		if err := os.WriteFile(tokenPath, []byte(body), 0o644); err != nil {
			return err
		}
		defer os.Remove(tokenPath)
		if _, err := c.Accept(ctx, chal); err != nil {
			return err
		}
		if _, err := c.WaitAuthorization(ctx, z.URI); err != nil {
			return err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain},
	}, key)
	if err != nil {
		return err
	}
	if _, err := c.WaitOrder(ctx, order.URI); err != nil {
		return err
	}
	ders, _, err := c.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return err
	}
	var chain []byte
	for _, d := range ders {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	dir := filepath.Join(m.CertDir, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// nginx runs as root in its master process and reads the key at reload.
	if err := writeFile(filepath.Join(dir, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "fullchain.pem"), chain, 0o644)
}

func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadOrCreateKey(path string) (crypto.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: bad PEM", path)
		}
		return x509.ParseECPrivateKey(blk.Bytes)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return key, writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}
