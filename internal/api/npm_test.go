package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/npm"
	"github.com/veksa/clowdr/internal/store"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (e *env) raw(t *testing.T, method, path, token string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

func npmPublishBody(name, version string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	pj := `{"name":"` + name + `","version":"` + version + `"}`
	tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(pj)), Typeflag: tar.TypeReg})
	tw.Write([]byte(pj))
	tw.Close()
	gz.Close()
	data := buf.Bytes()
	s512, s1 := sha512.Sum512(data), sha1.Sum(data)
	b, _ := json.Marshal(map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version,
			"dist": map[string]string{"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]), "shasum": hex.EncodeToString(s1[:])}}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{"data": base64.StdEncoding.EncodeToString(data), "length": len(data)}},
	})
	return b
}

// The npm registry through the whole API: admin token management, the token kinds kept apart,
// the feature flag for the CLI, and no token in any answer or log line.
func TestNPMThroughAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	shop := e.projectClient(t, "shop", "example.com")

	var v Version
	if err := e.admin.Do(ctx, "GET", "/v1/version", nil, &v); err != nil || len(v.Features) != 1 || v.Features[0] != "npm" {
		t.Fatalf("features: %+v %v", v, err)
	}

	var ci npm.TokenResponse
	if err := e.admin.Do(ctx, "POST", "/v1/npm/tokens", NPMTokenRequest{Name: "ci", Scopes: []string{"npm:publish:@shop/*"}, ExpiresIn: "90d"}, &ci); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ci.Token, "clwn_") || ci.Info.Hash != "" || ci.Info.Expires.Sub(ci.Info.Created).Hours() != 90*24 {
		t.Fatalf("%+v", ci)
	}
	for _, c := range []struct {
		req    NPMTokenRequest
		status int
	}{
		{NPMTokenRequest{Name: "ci", Scopes: []string{"plugins:read"}}, 409},
		{NPMTokenRequest{Name: "x", Scopes: []string{"npm:read:lodash"}}, 400},
		{NPMTokenRequest{Name: "x"}, 400},
		{NPMTokenRequest{Name: "x", Scopes: []string{"plugins:read"}, ExpiresIn: "soon"}, 400},
	} {
		var se *StatusError
		if err := e.admin.Do(ctx, "POST", "/v1/npm/tokens", c.req, nil); !errors.As(err, &se) || se.Status != c.status {
			t.Errorf("%+v: %v, want %d", c.req, err, c.status)
		}
	}

	// Admin and project tokens are no tokens on /npm/; npm tokens are none on /v1/ and /v2/.
	for _, tok := range []string{e.admin.Token, shop.Token} {
		for _, p := range []string{"/npm/-/whoami", "/npm/@shop/tool"} {
			if st, h, _ := e.raw(t, "GET", p, tok, nil); st != 401 || h.Get("WWW-Authenticate") == "" {
				t.Errorf("GET %s with a non-npm token: %d", p, st)
			}
		}
	}
	if st, _, _ := e.raw(t, "GET", "/v2/", ci.Token, nil); st != 401 {
		t.Errorf("/v2/ with an npm token: %d", st)
	}
	if st, _, _ := e.raw(t, "GET", "/v2/shop/server/tags/list", ci.Token, nil); st != 401 {
		t.Errorf("/v2/ with an npm token: %d", st)
	}

	st, _, body := e.raw(t, "PUT", "/npm/@shop%2ftool", ci.Token, npmPublishBody("@shop/tool", "1.0.0"))
	if st != 201 {
		t.Fatalf("publish: %d %s", st, body)
	}
	if st, _, body := e.raw(t, "GET", "/npm/@shop%2ftool", ci.Token, nil); st != 200 || !strings.Contains(string(body), e.srv.URL+"/npm/@shop/tool/-/tool-1.0.0.tgz") {
		t.Fatalf("document: %d %s", st, body)
	}
	if st, _, _ := e.raw(t, "GET", "/npm/@shop/tool/-/tool-1.0.0.tgz", ci.Token, nil); st != 200 {
		t.Fatalf("tarball: %d", st)
	}
	// Large bodies pass the API's 1 MiB cap: /npm/ has its own.
	if st, _, _ := e.raw(t, "GET", "/npm/@shop/tool/../../v1/projects", ci.Token, nil); st == 200 {
		t.Fatal("path traversal out of /npm/ reached the API")
	}

	var ts []store.NPMToken
	if err := e.admin.Do(ctx, "GET", "/v1/npm/tokens", nil, &ts); err != nil || len(ts) != 1 || ts[0].Hash != "" || ts[0].LastUsed.IsZero() {
		t.Fatalf("list: %+v %v", ts, err)
	}
	var rot npm.TokenResponse
	if err := e.admin.Do(ctx, "POST", "/v1/npm/tokens/ci/rotate", nil, &rot); err != nil || rot.Info.RotatedFrom != ci.Info.ID {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	if st, _, _ := e.raw(t, "GET", "/npm/-/whoami", ci.Token, nil); st != 200 {
		t.Fatalf("old token in grace: %d", st)
	}
	var now npm.TokenResponse
	if err := e.admin.Do(ctx, "POST", "/v1/npm/tokens/ci/rotate", npm.RotateRequest{Now: true}, &now); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.raw(t, "GET", "/npm/-/whoami", rot.Token, nil); st != 401 {
		t.Fatalf("token rotated with now still works: %d", st)
	}
	if err := e.admin.Do(ctx, "DELETE", "/v1/npm/tokens/ci", nil, nil); err != nil {
		t.Fatal(err)
	}
	var se *StatusError
	if err := e.admin.Do(ctx, "DELETE", "/v1/npm/tokens/ci", nil, nil); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("revoke twice: %v", err)
	}
	for _, tok := range []string{ci.Token, rot.Token, now.Token} {
		if st, _, _ := e.raw(t, "GET", "/npm/-/whoami", tok, nil); st != 401 {
			t.Fatalf("revoked token works: %d", st)
		}
	}

	var audit []store.NPMAudit
	if err := e.admin.Do(ctx, "GET", "/v1/npm/audit?package=@shop/tool", nil, &audit); err != nil || len(audit) != 3 {
		t.Fatalf("audit: %+v %v", audit, err)
	}
	if audit[2].Action != "publish" || audit[2].Token != "ci" || audit[2].Addr != "127.0.0.1" {
		t.Fatalf("audit entry: %+v", audit[2])
	}
	if err := e.admin.Do(ctx, "GET", "/v1/npm/audit?action=token&limit=2", nil, &audit); err != nil || len(audit) != 2 || audit[0].Action != "token.revoke" {
		t.Fatalf("token audit: %+v %v", audit, err)
	}

	for _, tok := range []string{ci.Token, rot.Token, now.Token} {
		if strings.Contains(e.logs.String(), tok) {
			t.Fatal("an npm token is in the server log")
		}
		if strings.Contains(e.logs.String(), tok[len(tok)-48:]) {
			t.Fatal("an npm token's secret is in the server log")
		}
	}
}

// A request through nginx that did not come from the deploy host's location reaches the ACME
// challenges only: a traversal that lands in an ACME location of any site must not open the API.
func TestSocketReachesOnlyACME(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	old := ACMEDir
	ACMEDir = dir
	t.Cleanup(func() { ACMEDir = old })
	if err := os.WriteFile(filepath.Join(dir, "tok_1"), []byte("proof"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := e.api.Handler()
	serve := func(target string, marked bool) (int, string) {
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr = "@"
		r.Header.Set("Authorization", "Bearer "+e.admin.Token)
		if marked {
			r.Header.Set(ingress.ViaHeader, ingress.ViaDeployHost)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := serve("/.well-known/acme-challenge/tok_1", false); code != 200 || body != "proof" {
		t.Fatalf("challenge: %d %s", code, body)
	}
	for _, p := range []string{"/v1/projects", "/v1/version", "/npm/-/whoami", "/v2/",
		"/npm/x/..%2F..%2F.well-known/acme-challenge/tok_1", "/v1/x/..%2F..%2F.well-known/acme-challenge/tok_1"} {
		if code, _ := serve(p, false); code != 404 {
			t.Errorf("%s through a non-deploy location: %d", p, code)
		}
	}
	if code, _ := serve("/v1/projects", true); code != 200 {
		t.Fatalf("deploy host: %d", code)
	}
}
