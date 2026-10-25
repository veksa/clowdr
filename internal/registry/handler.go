package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Handler serves /v2/.
type Handler struct {
	Store  Store
	Owners Ownership
	// Authorize returns the project the request may access ("*" = any, admin) or an error.
	Authorize func(r *http.Request) (string, error)
	// OnTag is called after repo:tag was pushed.
	OnTag func(ctx context.Context, repo, tag string, desc Descriptor)
	Log   *slog.Logger

	mu      sync.Mutex
	uploads map[string]*upload
}

type upload struct {
	project string
	repo    string
	created time.Time
	mu      sync.Mutex
}

const maxManifest = 4 << 20

var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// ValidTag reports whether s is a tag as the distribution spec defines it (no digest, no path).
func ValidTag(s string) bool { return tagRe.MatchString(s) }

const (
	mtManifestV2   = "application/vnd.docker.distribution.manifest.v2+json"
	mtManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex     = "application/vnd.oci.image.index.v1+json"
)

var manifestTypes = map[string]bool{mtManifestV2: true, mtManifestList: true, mtOCIManifest: true, mtOCIIndex: true}

type regError struct {
	status int
	code   string
	msg    string
}

func writeErr(w http.ResponseWriter, e regError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": e.code, "message": e.msg}}})
}

var (
	errNameUnknown     = regError{404, "NAME_UNKNOWN", "repository name not known to registry"}
	errNameInvalid     = regError{400, "NAME_INVALID", "invalid repository name"}
	errBlobUnknown     = regError{404, "BLOB_UNKNOWN", "blob unknown to registry"}
	errUploadUnknown   = regError{404, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry"}
	errDigestInvalid   = regError{400, "DIGEST_INVALID", "provided digest did not match uploaded content"}
	errManifestUnknown = regError{404, "MANIFEST_UNKNOWN", "manifest unknown"}
	errManifestInvalid = regError{400, "MANIFEST_INVALID", "manifest invalid"}
	errManifestBlob    = regError{400, "MANIFEST_BLOB_UNKNOWN", "manifest references a blob unknown to registry"}
	errTagInvalid      = regError{400, "TAG_INVALID", "invalid tag"}
	errUnsupported     = regError{405, "UNSUPPORTED", "the operation is unsupported"}
	errDenied          = regError{403, "DENIED", "requested access to the resource is denied"}
	errRange           = regError{416, "BLOB_UPLOAD_INVALID", "invalid content range"}
)

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	scope, err := h.Authorize(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="clowdr"`)
		writeErr(w, regError{401, "UNAUTHORIZED", "authentication required"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if rest == "" || rest == r.URL.Path {
		w.WriteHeader(200)
		return
	}

	var name, kind, ref string
	for _, k := range []string{"/blobs/uploads/", "/blobs/uploads", "/blobs/", "/manifests/", "/tags/list"} {
		if i := strings.LastIndex(rest, k); i > 0 {
			name, kind, ref = rest[:i], strings.TrimSuffix(k, "/"), rest[i+len(k):]
			break
		}
	}
	if name == "" {
		writeErr(w, errUnsupported)
		return
	}
	if !nameRe.MatchString(name) || !strings.Contains(name, "/") {
		writeErr(w, errNameInvalid)
		return
	}
	project, _, _ := strings.Cut(name, "/")
	if scope != "*" && scope != project {
		writeErr(w, errDenied)
		return
	}

	switch kind {
	case "/blobs":
		h.blob(w, r, project, ref)
	case "/blobs/uploads":
		h.uploadReq(w, r, project, name, ref)
	case "/manifests":
		h.manifest(w, r, project, name, ref)
	case "/tags/list":
		h.tags(w, r, name)
	}
}

func (h *Handler) owns(project, d string) bool {
	if h.Owners == nil {
		return true
	}
	ok, err := h.Owners.Owns(project, d)
	if err != nil {
		h.log().Error("registry ownership", "err", err)
	}
	return ok
}

func (h *Handler) own(project string, ds ...string) error {
	if h.Owners == nil {
		return nil
	}
	return h.Owners.Own(project, ds...)
}

func (h *Handler) blob(w http.ResponseWriter, r *http.Request, project, d string) {
	if !ValidDigest(d) {
		writeErr(w, errDigestInvalid)
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
	case http.MethodDelete:
		writeErr(w, errUnsupported)
		return
	default:
		w.WriteHeader(405)
		return
	}
	if !h.owns(project, d) {
		writeErr(w, errBlobUnknown)
		return
	}
	if r.Method == http.MethodHead {
		size, err := h.Store.Stat(r.Context(), d)
		if err != nil {
			writeErr(w, errBlobUnknown)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(200)
		return
	}
	rc, size, err := h.Store.Reader(r.Context(), d)
	if err != nil {
		writeErr(w, errBlobUnknown)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Docker-Content-Digest", d)
	io.Copy(w, rc)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Handler) getUpload(project, id string) *upload {
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.uploads[id]
	if u == nil || u.project != project {
		return nil
	}
	return u
}

func (h *Handler) uploadReq(w http.ResponseWriter, r *http.Request, project, name, id string) {
	ctx := r.Context()
	if id == "" {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		q := r.URL.Query()
		// Cross-repo mount: blobs are global, so it only needs ownership.
		if m := q.Get("mount"); m != "" && ValidDigest(m) && h.owns(project, m) {
			if _, err := h.Store.Stat(ctx, m); err == nil {
				w.Header().Set("Location", "/v2/"+name+"/blobs/"+m)
				w.Header().Set("Docker-Content-Digest", m)
				w.WriteHeader(201)
				return
			}
		}
		id = newID()
		h.mu.Lock()
		if h.uploads == nil {
			h.uploads = map[string]*upload{}
		}
		h.uploads[id] = &upload{project: project, repo: name, created: time.Now()}
		h.mu.Unlock()
		if d := q.Get("digest"); d != "" { // monolithic upload
			h.finish(w, r, project, name, id, d)
			return
		}
		h.accepted(w, name, id, 0)
		return
	}

	u := h.getUpload(project, id)
	if u == nil {
		writeErr(w, errUploadUnknown)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		wr, err := h.Store.Writer(ctx, id)
		if err != nil {
			writeErr(w, errUploadUnknown)
			return
		}
		off := wr.Offset()
		wr.Close()
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max(off-1, 0)))
		w.Header().Set("Docker-Upload-UUID", id)
		w.WriteHeader(204)
	case http.MethodPatch:
		wr, err := h.Store.Writer(ctx, id)
		if err != nil {
			h.log().Error("registry writer", "err", err)
			writeErr(w, errUploadUnknown)
			return
		}
		defer wr.Close()
		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, _, _ := strings.Cut(strings.TrimPrefix(cr, "bytes="), "-")
			if n, err := strconv.ParseInt(start, 10, 64); err != nil || n != wr.Offset() {
				writeErr(w, errRange)
				return
			}
		}
		if _, err := io.Copy(wr, r.Body); err != nil {
			h.log().Warn("registry upload interrupted", "id", id, "err", err)
		}
		h.accepted(w, name, id, wr.Offset())
	case http.MethodPut:
		h.finish(w, r, project, name, id, r.URL.Query().Get("digest"))
	case http.MethodDelete:
		h.Store.Abort(ctx, id)
		h.mu.Lock()
		delete(h.uploads, id)
		h.mu.Unlock()
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

func (h *Handler) accepted(w http.ResponseWriter, name, id string, off int64) {
	w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
	w.Header().Set("Range", fmt.Sprintf("0-%d", max(off-1, 0)))
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(202)
}

func (h *Handler) finish(w http.ResponseWriter, r *http.Request, project, name, id, d string) {
	ctx := r.Context()
	if !ValidDigest(d) {
		writeErr(w, errDigestInvalid)
		return
	}
	wr, err := h.Store.Writer(ctx, id)
	if err != nil {
		writeErr(w, errUploadUnknown)
		return
	}
	defer wr.Close()
	if _, err := io.Copy(wr, r.Body); err != nil {
		writeErr(w, regError{400, "BLOB_UPLOAD_INVALID", err.Error()})
		return
	}
	if err := wr.Commit(ctx, d); err != nil {
		h.Store.Abort(ctx, id)
		h.dropUpload(id)
		if errors.Is(err, ErrDigestInvalid) {
			writeErr(w, errDigestInvalid)
			return
		}
		h.log().Error("registry commit", "err", err)
		writeErr(w, regError{500, "UNKNOWN", err.Error()})
		return
	}
	h.dropUpload(id)
	if err := h.own(project, d); err != nil {
		writeErr(w, regError{500, "UNKNOWN", err.Error()})
		return
	}
	w.Header().Set("Location", "/v2/"+name+"/blobs/"+d)
	w.Header().Set("Docker-Content-Digest", d)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(201)
}

func (h *Handler) dropUpload(id string) {
	h.mu.Lock()
	delete(h.uploads, id)
	h.mu.Unlock()
}

// ExpireUploads aborts uploads older than ttl (interrupted `docker push`).
func (h *Handler) ExpireUploads(ctx context.Context, ttl time.Duration) {
	h.mu.Lock()
	var old []string
	for id, u := range h.uploads {
		if time.Since(u.created) >= ttl {
			old = append(old, id)
			delete(h.uploads, id)
		}
	}
	h.mu.Unlock()
	for _, id := range old {
		h.Store.Abort(ctx, id)
	}
}

type manifestDoc struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        *Descriptor  `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Manifests     []Descriptor `json:"manifests"`
}

func (h *Handler) manifest(w http.ResponseWriter, r *http.Request, project, name, ref string) {
	ctx := r.Context()
	isDigest := strings.HasPrefix(ref, "sha256:")
	if isDigest && !ValidDigest(ref) || !isDigest && !tagRe.MatchString(ref) {
		writeErr(w, errTagInvalid)
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		var desc Descriptor
		if isDigest {
			if !h.owns(project, ref) {
				writeErr(w, errManifestUnknown)
				return
			}
			size, err := h.Store.Stat(ctx, ref)
			if err != nil {
				writeErr(w, errManifestUnknown)
				return
			}
			desc = Descriptor{Digest: ref, Size: size}
		} else {
			var err error
			if desc, err = h.Store.Resolve(ctx, name, ref); err != nil {
				writeErr(w, errManifestUnknown)
				return
			}
		}
		rc, size, err := h.Store.Reader(ctx, desc.Digest)
		if err != nil {
			writeErr(w, errManifestUnknown)
			return
		}
		defer rc.Close()
		body, err := io.ReadAll(io.LimitReader(rc, maxManifest))
		if err != nil {
			writeErr(w, regError{500, "UNKNOWN", err.Error()})
			return
		}
		mt := desc.MediaType
		if mt == "" {
			var m manifestDoc
			json.Unmarshal(body, &m)
			mt = m.MediaType
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", desc.Digest)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(200)
		if r.Method == http.MethodGet {
			w.Write(body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxManifest+1))
		if err != nil || len(body) > maxManifest {
			writeErr(w, errManifestInvalid)
			return
		}
		var m manifestDoc
		if err := json.Unmarshal(body, &m); err != nil || m.SchemaVersion != 2 {
			writeErr(w, errManifestInvalid)
			return
		}
		mt := r.Header.Get("Content-Type")
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
		if !manifestTypes[mt] {
			mt = m.MediaType
		}
		if !manifestTypes[mt] {
			writeErr(w, errManifestInvalid)
			return
		}
		var refs []Descriptor
		if m.Config != nil {
			refs = append(refs, *m.Config)
		}
		refs = append(refs, m.Layers...)
		refs = append(refs, m.Manifests...)
		for _, d := range refs {
			if !ValidDigest(d.Digest) || !h.owns(project, d.Digest) {
				writeErr(w, errManifestBlob)
				return
			}
			if _, err := h.Store.Stat(ctx, d.Digest); err != nil {
				writeErr(w, errManifestBlob)
				return
			}
		}
		desc := Descriptor{MediaType: mt, Digest: Digest(body), Size: int64(len(body))}
		if isDigest && ref != desc.Digest {
			writeErr(w, errDigestInvalid)
			return
		}
		tag := ref
		if isDigest {
			tag = ""
		}
		if err := h.Store.PutManifest(ctx, name, tag, desc, body, refs); err != nil {
			h.log().Error("registry put manifest", "repo", name, "err", err)
			writeErr(w, regError{500, "UNKNOWN", err.Error()})
			return
		}
		if err := h.own(project, desc.Digest); err != nil {
			writeErr(w, regError{500, "UNKNOWN", err.Error()})
			return
		}
		if tag != "" && h.OnTag != nil {
			h.OnTag(ctx, name, tag, desc)
		}
		w.Header().Set("Location", "/v2/"+name+"/manifests/"+desc.Digest)
		w.Header().Set("Docker-Content-Digest", desc.Digest)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(201)
	case http.MethodDelete:
		writeErr(w, errUnsupported)
	default:
		w.WriteHeader(405)
	}
}

func (h *Handler) tags(w http.ResponseWriter, r *http.Request, name string) {
	tags, err := h.Store.Tags(r.Context(), name)
	if err != nil {
		writeErr(w, regError{500, "UNKNOWN", err.Error()})
		return
	}
	if len(tags) == 0 {
		writeErr(w, errNameUnknown)
		return
	}
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]any{"name": name, "tags": tags})
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}
