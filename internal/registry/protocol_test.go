package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// proto speaks the distribution API by hand, to cover what docker and go-containerregistry
// rarely do: chunked uploads, resumes, bad ranges and digests, mounts, aborted uploads.
type proto struct {
	t   *testing.T
	h   *Handler
	srv *httptest.Server
}

func newProto(t *testing.T, store Store) *proto {
	h := &Handler{
		Store:  store,
		Owners: &memOwners{m: map[string]bool{}},
		Authorize: func(r *http.Request) (string, error) {
			u, p, ok := r.BasicAuth()
			switch {
			case ok && u == "admin" && p == "secret-admin":
				return "*", nil
			case ok && p == "secret-"+u:
				return u, nil
			}
			return "", errors.New("no")
		},
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &proto{t: t, h: h, srv: srv}
}

// do sends a request as project (empty = anonymous) and returns the response with its body read.
func (p *proto) do(project, method, path string, body []byte, hdr ...string) (*http.Response, string) {
	p.t.Helper()
	u := path
	if !strings.HasPrefix(u, "http") {
		u = p.srv.URL + path
	}
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	if project != "" {
		req.SetBasicAuth(project, "secret-"+project)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (p *proto) expect(resp *http.Response, body string, status int, code string) {
	p.t.Helper()
	if resp.StatusCode != status || code != "" && !strings.Contains(body, `"`+code+`"`) {
		p.t.Fatalf("%s %s: want %d %s, got %d %s", resp.Request.Method, resp.Request.URL.Path, status, code, resp.StatusCode, body)
	}
}

// upload pushes blob in chunks and returns its digest.
func (p *proto) upload(project, repo string, blob []byte, chunk int) string {
	p.t.Helper()
	resp, body := p.do(project, "POST", "/v2/"+repo+"/blobs/uploads/", nil)
	p.expect(resp, body, 202, "")
	loc := resp.Header.Get("Location")
	for off := 0; off < len(blob); off += chunk {
		end := min(off+chunk, len(blob))
		resp, body = p.do(project, "PATCH", loc, blob[off:end], "Content-Range", fmt.Sprintf("%d-%d", off, end-1))
		p.expect(resp, body, 202, "")
		if want := fmt.Sprintf("0-%d", end-1); resp.Header.Get("Range") != want {
			p.t.Fatalf("Range %q, want %q", resp.Header.Get("Range"), want)
		}
	}
	d := Digest(blob)
	resp, body = p.do(project, "PUT", loc+"?digest="+d, nil)
	p.expect(resp, body, 201, "")
	if resp.Header.Get("Docker-Content-Digest") != d {
		p.t.Fatal("no digest header")
	}
	return d
}

func testProtocol(t *testing.T, store Store) {
	p := newProto(t, store)
	blob := bytes.Repeat([]byte("clowdr layer "), 1000)

	// Version check and auth.
	resp, body := p.do("shop", "GET", "/v2/", nil)
	p.expect(resp, body, 200, "")
	resp, body = p.do("", "GET", "/v2/", nil)
	p.expect(resp, body, 401, "UNAUTHORIZED")
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("no WWW-Authenticate")
	}
	resp, body = p.do("shop", "GET", "/v2/blog/web/tags/list", nil)
	p.expect(resp, body, 403, "DENIED")
	resp, body = p.do("shop", "GET", "/v2/Shop/Web/tags/list", nil)
	p.expect(resp, body, 400, "NAME_INVALID")
	resp, body = p.do("shop", "GET", "/v2/shop/catalog", nil)
	p.expect(resp, body, 405, "UNSUPPORTED")

	// Chunked upload, then HEAD and GET.
	d := p.upload("shop", "shop/web", blob, 4096)
	resp, body = p.do("shop", "HEAD", "/v2/shop/web/blobs/"+d, nil)
	p.expect(resp, body, 200, "")
	if resp.ContentLength != int64(len(blob)) {
		t.Fatalf("HEAD size %d", resp.ContentLength)
	}
	resp, body = p.do("shop", "GET", "/v2/shop/web/blobs/"+d, nil)
	p.expect(resp, body, 200, "")
	if body != string(blob) {
		t.Fatal("GET returned other content")
	}
	resp, body = p.do("shop", "DELETE", "/v2/shop/web/blobs/"+d, nil)
	p.expect(resp, body, 405, "UNSUPPORTED")
	resp, body = p.do("shop", "GET", "/v2/shop/web/blobs/sha256:xyz", nil)
	p.expect(resp, body, 400, "DIGEST_INVALID")

	// Another project does not see the blob, even though it is stored once.
	resp, body = p.do("blog", "HEAD", "/v2/blog/web/blobs/"+d, nil)
	p.expect(resp, body, 404, "")
	resp, body = p.do("blog", "GET", "/v2/blog/web/blobs/"+d, nil)
	p.expect(resp, body, 404, "BLOB_UNKNOWN")
	// ...and cannot mount it: it gets a fresh upload instead.
	resp, body = p.do("blog", "POST", "/v2/blog/web/blobs/uploads/?mount="+d+"&from=shop/web", nil)
	p.expect(resp, body, 202, "")
	// The same project mounts it into another repository.
	resp, body = p.do("shop", "POST", "/v2/shop/api/blobs/uploads/?mount="+d+"&from=shop/web", nil)
	p.expect(resp, body, 201, "")

	// Monolithic upload with a wrong digest is rejected and the upload is gone.
	other := []byte("other blob")
	resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/?digest="+Digest([]byte("x")), other)
	p.expect(resp, body, 400, "DIGEST_INVALID")
	resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/?digest="+Digest(other), other)
	p.expect(resp, body, 201, "")

	// Resumable upload: status, a bad range, resume, an upload of another project, abort.
	resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/", nil)
	loc := resp.Header.Get("Location")
	resp, body = p.do("shop", "PATCH", loc, []byte("hello "), "Content-Range", "0-5")
	p.expect(resp, body, 202, "")
	resp, body = p.do("shop", "GET", loc, nil)
	p.expect(resp, body, 204, "")
	if resp.Header.Get("Range") != "0-5" {
		t.Fatalf("status Range %q", resp.Header.Get("Range"))
	}
	resp, body = p.do("shop", "PATCH", loc, []byte("again"), "Content-Range", "0-4")
	p.expect(resp, body, 416, "BLOB_UPLOAD_INVALID")
	resp, body = p.do("blog", "PATCH", strings.Replace(loc, "/shop/web/", "/blog/web/", 1), []byte("x"))
	p.expect(resp, body, 404, "BLOB_UPLOAD_UNKNOWN")
	resp, body = p.do("shop", "PUT", loc+"?digest="+Digest([]byte("hello world")), []byte("world"))
	p.expect(resp, body, 201, "")
	resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/", nil)
	loc = resp.Header.Get("Location")
	resp, body = p.do("shop", "DELETE", loc, nil)
	p.expect(resp, body, 204, "")
	resp, body = p.do("shop", "PATCH", loc, []byte("x"))
	p.expect(resp, body, 404, "BLOB_UPLOAD_UNKNOWN")

	// Interrupted pushes expire.
	resp, _ = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/", nil)
	loc = resp.Header.Get("Location")
	p.do("shop", "PATCH", loc, []byte("partial"))
	p.h.ExpireUploads(context.Background(), 0)
	resp, body = p.do("shop", "GET", loc, nil)
	p.expect(resp, body, 404, "BLOB_UPLOAD_UNKNOWN")

	// Manifests must reference blobs the project owns, and match their digest.
	cfg := p.upload("shop", "shop/web", []byte(`{"architecture":"amd64","os":"linux"}`), 1<<20)
	manifest := func(layers ...string) []byte {
		var ls []Descriptor
		for _, l := range layers {
			ls = append(ls, Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: l, Size: 1})
		}
		b, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": mtOCIManifest,
			"config": Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfg, Size: 1},
			"layers": ls,
		})
		return b
	}
	ct := []string{"Content-Type", mtOCIManifest}
	resp, body = p.do("shop", "PUT", "/v2/shop/web/manifests/v1", manifest(Digest([]byte("never uploaded"))), ct...)
	p.expect(resp, body, 400, "MANIFEST_BLOB_UNKNOWN")
	resp, body = p.do("shop", "PUT", "/v2/shop/web/manifests/v1", []byte("{not json"), ct...)
	p.expect(resp, body, 400, "MANIFEST_INVALID")
	resp, body = p.do("shop", "PUT", "/v2/shop/web/manifests/v1", []byte(`{"schemaVersion":2}`), "Content-Type", "text/plain")
	p.expect(resp, body, 400, "MANIFEST_INVALID")
	resp, body = p.do("shop", "PUT", "/v2/shop/web/manifests/bad:tag", manifest(), ct...)
	p.expect(resp, body, 400, "TAG_INVALID")
	m := manifest()
	resp, body = p.do("shop", "PUT", "/v2/shop/web/manifests/"+Digest([]byte("other")), m, ct...)
	p.expect(resp, body, 400, "DIGEST_INVALID")
	resp, body = p.do("shop", "PUT", "/v2/shop/web/manifests/"+Digest(m), m, ct...) // by digest, no tag
	p.expect(resp, body, 201, "")
	resp, body = p.do("shop", "GET", "/v2/shop/web/manifests/"+Digest(m), nil)
	p.expect(resp, body, 200, "")
	if body != string(m) || resp.Header.Get("Content-Type") != mtOCIManifest {
		t.Fatalf("manifest by digest: %s %s", resp.Header.Get("Content-Type"), body)
	}
	resp, body = p.do("blog", "GET", "/v2/blog/web/manifests/"+Digest(m), nil)
	p.expect(resp, body, 404, "MANIFEST_UNKNOWN")
	resp, body = p.do("shop", "DELETE", "/v2/shop/web/manifests/"+Digest(m), nil)
	p.expect(resp, body, 405, "UNSUPPORTED")
	resp, body = p.do("shop", "GET", "/v2/shop/web/manifests/missing", nil)
	p.expect(resp, body, 404, "MANIFEST_UNKNOWN")
	resp, body = p.do("shop", "GET", "/v2/shop/nothing/tags/list", nil)
	p.expect(resp, body, 404, "NAME_UNKNOWN")
}

func TestProtocolFS(t *testing.T) {
	testProtocol(t, &FSStore{Dir: t.TempDir()})

	// Tags (the containerd test below needs real layers for them, since it unpacks).
	p := newProto(t, &FSStore{Dir: t.TempDir()})
	for _, tag := range []string{"v2", "v1"} {
		img, _ := random.Image(64, 1)
		if err := remote.Write(refFor(t, p.srv, "shop/web:"+tag), img, as("shop")); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := p.do("shop", "GET", "/v2/shop/web/tags/list", nil)
	p.expect(resp, body, 200, "")
	if !strings.Contains(body, `"tags":["v1","v2"]`) {
		t.Fatal(body)
	}
	resp, body = p.do("admin", "GET", "/v2/shop/web/tags/list", nil) // admin sees every project
	p.expect(resp, body, 200, "")
}

// TestProtocolContainerd runs against containerd's content store on a Linux host with docker's
// containerd image store: CLOWDR_CONTAINERD=/run/containerd/containerd.sock (needs root; the e2e
// job runs it).
func TestProtocolContainerd(t *testing.T) {
	sock := os.Getenv("CLOWDR_CONTAINERD")
	if sock == "" {
		t.Skip("CLOWDR_CONTAINERD is not set")
	}
	s, err := NewContainerdStore(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.UploadTTL = time.Minute
	testProtocol(t, s)

	// A pushed image becomes a docker image, unpacked and ready to run.
	p := newProto(t, s)
	repo := fmt.Sprintf("clowdrtest/web%d", time.Now().UnixNano())
	img, _ := random.Image(256, 2)
	if err := remote.Write(refFor(t, p.srv, repo+":v1"), img, as("clowdrtest")); err != nil {
		t.Fatal(err)
	}
	ctx := s.ctx(context.Background())
	t.Cleanup(func() { s.Client.ImageService().Delete(ctx, ImagePrefix+repo+":v1", images.SynchronousDelete()) })
	got, err := s.Client.ImageService().Get(ctx, ImagePrefix+repo+":v1")
	if err != nil {
		t.Fatal("not a docker image:", err)
	}
	want, _ := img.Digest()
	if got.Target.Digest.String() != want.String() {
		t.Fatalf("image points at %s, pushed %s", got.Target.Digest, want)
	}
	tags, err := s.Tags(context.Background(), repo)
	if err != nil || len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("tags %v %v", tags, err)
	}
	// Pull it back through the registry.
	pulled, err := remote.Image(refFor(t, p.srv, repo+":v1"), as("clowdrtest"))
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := pulled.Digest(); d != want {
		t.Fatalf("pulled %s", d)
	}
	layers, _ := pulled.Layers()
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, rc)
		rc.Close()
	}
}

func refFor(t *testing.T, srv *httptest.Server, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://")+"/"+s, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUploadLimits(t *testing.T) {
	p := newProto(t, &FSStore{Dir: t.TempDir()})
	p.h.MaxBlob = func() int64 { return 8 }

	// A blob over the cap is refused and its upload dropped, chunked or monolithic.
	resp, _ := p.do("shop", "POST", "/v2/shop/web/blobs/uploads/", nil)
	loc := resp.Header.Get("Location")
	resp, body := p.do("shop", "PATCH", loc, []byte("12345"))
	p.expect(resp, body, 202, "")
	resp, body = p.do("shop", "PATCH", loc, []byte("6789"))
	p.expect(resp, body, 413, "SIZE_INVALID")
	resp, body = p.do("shop", "GET", loc, nil)
	p.expect(resp, body, 404, "BLOB_UPLOAD_UNKNOWN")
	big := []byte("123456789")
	resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/?digest="+Digest(big), big)
	p.expect(resp, body, 413, "SIZE_INVALID")
	p.upload("shop", "shop/web", []byte("12345678"), 4) // exactly the cap is fine

	// Open uploads per project are capped; other projects are not affected.
	for i := 0; i < maxUploads; i++ {
		resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/", nil)
		p.expect(resp, body, 202, "")
	}
	resp, body = p.do("shop", "POST", "/v2/shop/web/blobs/uploads/", nil)
	p.expect(resp, body, 429, "TOOMANYREQUESTS")
	resp, body = p.do("blog", "POST", "/v2/blog/web/blobs/uploads/", nil)
	p.expect(resp, body, 202, "")

	// A nearly full disk pauses pushes.
	p.h.LowDisk = func() error { return errors.New("disk almost full") }
	resp, body = p.do("blog", "PATCH", resp.Header.Get("Location"), []byte("x"))
	p.expect(resp, body, 507, "DENIED")
}
