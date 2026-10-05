package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type memOwners struct {
	mu sync.Mutex
	m  map[string]bool
}

func (o *memOwners) Owns(p, d string) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.m[p+"|"+d], nil
}

func (o *memOwners) Own(p string, ds ...string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range ds {
		o.m[p+"|"+d] = true
	}
	return nil
}

type env struct {
	srv     *httptest.Server
	uploads atomic.Int32 // committed blob uploads
	tagged  []string
}

func newEnv(t *testing.T) *env {
	e := &env{}
	h := &Handler{
		Store:  &FSStore{Dir: t.TempDir()},
		Owners: &memOwners{m: map[string]bool{}},
		Authorize: func(r *http.Request) (string, error) {
			u, p, ok := r.BasicAuth()
			if !ok || p != "secret-"+u {
				return "", errors.New("no")
			}
			return u, nil
		},
		OnTag: func(_ context.Context, repo, tag string, _ Descriptor) { e.tagged = append(e.tagged, repo+":"+tag) },
	}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			e.uploads.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) ref(t *testing.T, s string) name.Reference {
	r, err := name.ParseReference(strings.TrimPrefix(e.srv.URL, "http://")+"/"+s, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func as(project string) remote.Option {
	return remote.WithAuth(&authn.Basic{Username: project, Password: "secret-" + project})
}

func TestPushPull(t *testing.T) {
	e := newEnv(t)
	img, err := random.Image(1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(e.ref(t, "shop/server:v1"), img, as("shop")); err != nil {
		t.Fatal(err)
	}
	first := e.uploads.Load() // 3 layers + config
	if first != 4 {
		t.Fatalf("uploads = %d", first)
	}
	got, err := remote.Image(e.ref(t, "shop/server:v1"), as("shop"))
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := img.Digest()
	d2, _ := got.Digest()
	if d1 != d2 {
		t.Fatal("digest mismatch")
	}
	if len(e.tagged) != 1 || e.tagged[0] != "shop/server:v1" {
		t.Fatal(e.tagged)
	}

	// Next release: one new layer on top, only it (and the new config) is uploaded.
	extra, _ := random.Layer(2048, "application/vnd.docker.image.rootfs.diff.tar.gzip")
	img2, err := mutate.AppendLayers(img, extra)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(e.ref(t, "shop/server:v2"), img2, as("shop")); err != nil {
		t.Fatal(err)
	}
	if n := e.uploads.Load() - first; n != 2 {
		t.Fatalf("second push uploaded %d blobs, want 2", n)
	}
}

func TestIsolation(t *testing.T) {
	e := newEnv(t)
	img, _ := random.Image(512, 1)
	if err := remote.Write(e.ref(t, "shop/server:v1"), img, as("shop")); err != nil {
		t.Fatal(err)
	}
	// Another project cannot read shop's repo.
	if _, err := remote.Image(e.ref(t, "shop/server:v1"), as("blog")); err == nil {
		t.Fatal("blog read shop")
	}
	// Nor reach its layers through its own repo by digest.
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	dref, _ := name.NewDigest(strings.TrimPrefix(e.srv.URL, "http://")+"/blog/web@"+ld.String(), name.Insecure)
	if l, err := remote.Layer(dref, as("blog")); err == nil {
		if rc, err := l.Compressed(); err == nil {
			_, err = io.ReadAll(rc)
			rc.Close()
			if err == nil {
				t.Fatal("blog read a shop layer by digest")
			}
		}
	}
	// Pushing the same image as blog works (uploads again, stored once).
	if err := remote.Write(e.ref(t, "blog/web:v1"), img, as("blog")); err != nil {
		t.Fatal(err)
	}
	// Bad password.
	bad := remote.WithAuth(&authn.Basic{Username: "blog", Password: "x"})
	if err := remote.Write(e.ref(t, "blog/web:v2"), img, bad); err == nil {
		t.Fatal("bad password accepted")
	}
}

func TestIndexAndDigestInvalid(t *testing.T) {
	e := newEnv(t)
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(e.ref(t, "shop/multi:v1"), idx, as("shop")); err != nil {
		t.Fatal(err)
	}
	got, err := remote.Index(e.ref(t, "shop/multi:v1"), as("shop"))
	if err != nil {
		t.Fatal(err)
	}
	var d1, d2 v1.Hash
	d1, _ = idx.Digest()
	d2, _ = got.Digest()
	if d1 != d2 {
		t.Fatal("index digest mismatch")
	}

	// Upload with a wrong digest is rejected.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v2/shop/x/blobs/uploads/?digest=sha256:"+strings.Repeat("0", 64), strings.NewReader("hello"))
	req.SetBasicAuth("shop", "secret-shop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
