package analytics

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 is a minimal S3 client (AWS Signature Version 4): PUT, GET, HEAD and presigned GET, enough
// for the archive. Any S3-compatible store works: AWS, R2, MinIO, DigitalOcean Spaces.
type S3 struct {
	Endpoint  string // https://s3.example.com
	Region    string // default us-east-1
	Bucket    string
	AccessKey string
	SecretKey string
	// Virtual puts the bucket in the host name (bucket.endpoint) instead of the path.
	Virtual bool
	HTTP    *http.Client
	// now is the signing clock (tests).
	now func() time.Time
}

// ErrNotFound is a missing object.
var ErrNotFound = errors.New("not found")

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (c *S3) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *S3) clock() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

// objectURL is the URL of a key, path-style or virtual-host style.
func (c *S3) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	p := "/" + uriEncode(key, false)
	if c.Virtual {
		u.Host = c.Bucket + "." + u.Host
	} else {
		p = "/" + uriEncode(c.Bucket, false) + p
	}
	u.Path, u.RawPath = "", ""
	u.Opaque = ""
	u.RawPath = p
	u.Path, _ = url.PathUnescape(p)
	return u, nil
}

// uriEncode encodes like AWS's UriEncode: unreserved characters stay, everything else is
// %XX in upper case; '/' stays unless encodeSlash.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (c *S3) signingKey(date string) []byte {
	k := hmacSHA256([]byte("AWS4"+c.SecretKey), date)
	k = hmacSHA256(k, c.region())
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

// canonicalQuery sorts and encodes query parameters as SigV4 wants them.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// signature computes the SigV4 signature of a request described by its parts. headers are
// lower-cased names to trimmed values and must include host.
func (c *S3) signature(method, path string, query url.Values, headers map[string]string, payloadHash string, t time.Time) (signed, sig string) {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signed = strings.Join(names, ";")
	canonical := strings.Join([]string{method, path, canonicalQuery(query), ch.String(), signed, payloadHash}, "\n")
	date := t.Format("20060102")
	scope := date + "/" + c.region() + "/s3/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", t.Format("20060102T150405Z"), scope, sha256Hex([]byte(canonical))}, "\n")
	return signed, hex.EncodeToString(hmacSHA256(c.signingKey(date), toSign))
}

// sign adds the SigV4 Authorization header to req; extra headers already on req that start with
// x-amz- (and content-type, range, date) are signed too.
func (c *S3) sign(req *http.Request, payloadHash string) {
	t := c.clock()
	req.Header.Set("X-Amz-Date", t.Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "range" || lk == "date" || lk == "content-md5" {
			headers[lk] = strings.Join(vs, ",")
		}
	}
	signed, sig := c.signature(req.Method, req.URL.EscapedPath(), req.URL.Query(), headers, payloadHash, t)
	scope := t.Format("20060102") + "/" + c.region() + "/s3/aws4_request"
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, sig))
}

func (c *S3) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *S3) do(ctx context.Context, method, key string, body []byte, contentType string) (*http.Response, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	hash := emptySHA256
	if body != nil {
		rd = bytes.NewReader(body)
		hash = sha256Hex(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.sign(req, hash)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 %s %s: %d %s", method, key, resp.StatusCode, s3Error(b))
	}
	return resp, nil
}

// s3Error pulls the code and message out of an S3 XML error.
func s3Error(b []byte) string {
	s := string(b)
	get := func(tag string) string {
		i := strings.Index(s, "<"+tag+">")
		j := strings.Index(s, "</"+tag+">")
		if i < 0 || j < i {
			return ""
		}
		return s[i+len(tag)+2 : j]
	}
	if code := get("Code"); code != "" {
		return strings.TrimSpace(code + " " + get("Message"))
	}
	// Not an S3 error: the endpoint is set by a project token, so whatever else answered there
	// (an internal service) does not get its body echoed into the archive status.
	return "(not an S3 error response)"
}

// Put uploads an object (private: no ACL is set).
func (c *S3) Put(ctx context.Context, key string, body []byte, contentType string) error {
	resp, err := c.do(ctx, "PUT", key, body, contentType)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get downloads an object; ErrNotFound when it is missing.
func (c *S3) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.do(ctx, "GET", key, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Head reports an object's size; ErrNotFound when it is missing.
func (c *S3) Head(ctx context.Context, key string) (int64, error) {
	resp, err := c.do(ctx, "HEAD", key, nil, "")
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	n, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return n, nil
}

// Presign returns a GET URL valid for expires (at most 7 days).
func (c *S3) Presign(key string, expires time.Duration) (string, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	t := c.clock()
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.AccessKey+"/"+t.Format("20060102")+"/"+c.region()+"/s3/aws4_request")
	q.Set("X-Amz-Date", t.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	_, sig := c.signature("GET", u.EscapedPath(), q, map[string]string{"host": u.Host}, "UNSIGNED-PAYLOAD", t)
	u.RawQuery = canonicalQuery(q) + "&X-Amz-Signature=" + sig
	return u.String(), nil
}
