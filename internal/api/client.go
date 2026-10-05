package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	Server string // https://deploy.example.com or https://203.0.113.10:7443
	Token  string
	// Fingerprint pins a self-signed server certificate (sha256 of the DER, hex).
	Fingerprint string
	HTTP        *http.Client
}

func NewClient(server, token, fingerprint string) *Client {
	c := &Client{Server: strings.TrimRight(server, "/"), Token: token, Fingerprint: fingerprint}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if fingerprint != "" {
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // replaced by the pin check below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no server certificate")
				}
				if got := CertFingerprint(cs.PeerCertificates[0]); !strings.EqualFold(got, fingerprint) {
					return fmt.Errorf("server certificate fingerprint %s does not match the pinned %s", got, fingerprint)
				}
				return nil
			},
		}
	}
	c.HTTP = &http.Client{Transport: tr}
	return c
}

func CertFingerprint(c *x509.Certificate) string {
	h := sha256.Sum256(c.Raw)
	return hex.EncodeToString(h[:])
}

// Host is the server host:port, used as the registry host for docker.
func (c *Client) Host() string {
	u, err := url.Parse(c.Server)
	if err != nil {
		return c.Server
	}
	return u.Host
}

func (c *Client) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, &StatusError{Status: resp.StatusCode, Message: e.Error}
	}
	return resp, nil
}

type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return http.StatusText(e.Status)
	}
	return e.Message
}

// Do sends a request and decodes the JSON response into out (may be nil).
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if s, ok := out.(*string); ok {
		b, err := io.ReadAll(resp.Body)
		*s = string(b)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream sends a request whose response is NDJSON events and calls fn for each progress
// message. It returns the final event's release and error.
func (c *Client) Stream(ctx context.Context, method, path string, body any, fn func(Event)) (*Event, error) {
	resp, err := c.req(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return nil, errors.New("connection closed before the operation finished (it continues on the server; check `clowctl releases`)")
			}
			return nil, err
		}
		if ev.Done {
			if ev.Error != "" {
				return &ev, errors.New(ev.Error)
			}
			return &ev, nil
		}
		fn(ev)
	}
}

// Logs streams log lines.
func (c *Client) Logs(ctx context.Context, path string, fn func(LogLine)) error {
	resp, err := c.req(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l LogLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			fn(l)
		}
	}
	return sc.Err()
}

func Path(parts ...string) string {
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/v1/" + strings.Join(parts, "/")
}

// Raw returns the response body of a GET for streaming.
func (c *Client) Raw(ctx context.Context, method, path string) (io.ReadCloser, error) {
	resp, err := c.req(ctx, method, path, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
