package analytics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The examples of the AWS documentation "Signature Calculations for the Authorization Header:
// Transferring Payload in a Single Chunk" and "Authenticating Requests: Using Query Parameters".
func awsExample() *S3 {
	return &S3{
		Endpoint: "https://s3.amazonaws.com", Bucket: "examplebucket", Virtual: true,
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
}

func TestSigV4GetObjectVector(t *testing.T) {
	c := awsExample()
	at := c.clock()
	signed, sig := c.signature("GET", "/test.txt", nil, map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": emptySHA256,
		"x-amz-date":           "20130524T000000Z",
	}, emptySHA256, at)
	if signed != "host;range;x-amz-content-sha256;x-amz-date" {
		t.Fatal(signed)
	}
	if sig != "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41" {
		t.Fatalf("signature %s", sig)
	}
}

func TestSigV4PutObjectVector(t *testing.T) {
	c := awsExample()
	body := "Welcome to Amazon S3."
	u, _ := c.objectURL("test$file.text")
	if u.EscapedPath() != "/test%24file.text" || u.Host != "examplebucket.s3.amazonaws.com" {
		t.Fatalf("url %s", u)
	}
	_, sig := c.signature("PUT", u.EscapedPath(), nil, map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"date":                 "Fri, 24 May 2013 00:00:00 GMT",
		"x-amz-content-sha256": sha256Hex([]byte(body)),
		"x-amz-date":           "20130524T000000Z",
		"x-amz-storage-class":  "REDUCED_REDUNDANCY",
	}, sha256Hex([]byte(body)), c.clock())
	if sig != "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd" {
		t.Fatalf("signature %s", sig)
	}
}

func TestSigV4PresignVector(t *testing.T) {
	c := awsExample()
	got, err := c.Presign("test.txt", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "https://examplebucket.s3.amazonaws.com/test.txt?") ||
		!strings.HasSuffix(got, "&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404") {
		t.Fatalf("presigned %s", got)
	}
	for _, part := range []string{"X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request", "X-Amz-Expires=86400", "X-Amz-SignedHeaders=host"} {
		if !strings.Contains(got, part) {
			t.Fatalf("missing %s in %s", part, got)
		}
	}
}

func TestS3PathStyleURL(t *testing.T) {
	c := &S3{Endpoint: "https://s3.example.com/", Bucket: "media"}
	u, _ := c.objectURL("analytics/ab12/v1/shop/raw/2026/11/2026-11-15.ndjson.zst")
	if u.String() != "https://s3.example.com/media/analytics/ab12/v1/shop/raw/2026/11/2026-11-15.ndjson.zst" {
		t.Fatal(u)
	}
}

// fakeS3 is an S3-compatible store in memory that checks every request's signature the way S3
// does: it recomputes it from the request with the secret it knows.
type fakeS3 struct {
	t       *testing.T
	secret  string
	objects map[string][]byte
	types   map[string]string
	fail    int // answer 500 to this many requests
	puts    int
	srv     *httptest.Server
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{t: t, secret: "s3cr3t", objects: map[string][]byte{}, types: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) client() *S3 {
	return &S3{Endpoint: f.srv.URL, Bucket: "bucket", AccessKey: "key", SecretKey: f.secret, Region: "auto"}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	if f.fail > 0 {
		f.fail--
		w.WriteHeader(500)
		w.Write([]byte("<Error><Code>InternalError</Code><Message>try again</Message></Error>"))
		return
	}
	check := &S3{SecretKey: f.secret, Region: "auto"}
	var want string
	if q := r.URL.Query(); q.Get("X-Amz-Signature") != "" {
		at, _ := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
		sig := q.Get("X-Amz-Signature")
		q.Del("X-Amz-Signature")
		_, want = check.signature(r.Method, r.URL.EscapedPath(), q, map[string]string{"host": r.Host}, "UNSIGNED-PAYLOAD", at)
		if sig != want {
			w.WriteHeader(403)
			return
		}
	} else {
		auth := r.Header.Get("Authorization")
		at, _ := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
		_, signedPart, _ := strings.Cut(auth, "SignedHeaders=")
		signed, _, _ := strings.Cut(signedPart, ",")
		headers := map[string]string{}
		for _, h := range strings.Split(signed, ";") {
			if h == "host" {
				headers[h] = r.Host
			} else {
				headers[h] = r.Header.Get(h)
			}
		}
		_, want = check.signature(r.Method, r.URL.EscapedPath(), r.URL.Query(), headers, r.Header.Get("X-Amz-Content-Sha256"), at)
		if !strings.HasSuffix(auth, "Signature="+want) || !strings.Contains(auth, "Credential=key/") {
			w.WriteHeader(403)
			w.Write([]byte("<Error><Code>SignatureDoesNotMatch</Code></Error>"))
			return
		}
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case "PUT":
		b := make([]byte, r.ContentLength)
		if _, err := ioReadFull(r, b); err != nil {
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("X-Amz-Content-Sha256") != sha256Hex(b) {
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("X-Amz-Acl") != "" {
			f.t.Errorf("objects must be private, got acl %s", r.Header.Get("X-Amz-Acl"))
		}
		f.objects[key] = b
		f.types[key] = r.Header.Get("Content-Type")
		f.puts++
	case "GET", "HEAD":
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", itoa(len(b)))
		if r.Method == "GET" {
			w.Write(b)
		}
	}
}

func TestS3AgainstFake(t *testing.T) {
	f := newFakeS3(t)
	c := f.client()
	ctx := context.Background()
	if err := c.Put(ctx, "a/b c.txt", []byte("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Head(ctx, "a/b c.txt"); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if b, err := c.Get(ctx, "a/b c.txt"); err != nil || string(b) != "hello" {
		t.Fatal(string(b), err)
	}
	if _, err := c.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	u, err := c.Presign("a/b c.txt", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("presigned GET: %v %v", resp, err)
	}
	resp.Body.Close()
	bad := f.client()
	bad.SecretKey = "wrong"
	if err := bad.Put(ctx, "x", []byte("x"), ""); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("a wrong secret must be refused: %v", err)
	}
	f.fail = 1
	if err := c.Put(ctx, "y", []byte("y"), ""); err == nil || !strings.Contains(err.Error(), "InternalError try again") {
		t.Fatalf("server error: %v", err)
	}
}

func TestS3ErrorEchoesOnlyS3Errors(t *testing.T) {
	if got := s3Error([]byte(`<Error><Code>AccessDenied</Code><Message>no</Message></Error>`)); got != "AccessDenied no" {
		t.Fatal(got)
	}
	if got := s3Error([]byte(`{"internal":"secret config"}`)); strings.Contains(got, "secret") {
		t.Fatalf("a foreign body echoed: %q", got)
	}
}
