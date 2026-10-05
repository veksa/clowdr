// Package registry implements the push/pull side of the OCI Distribution API on top of a
// blob store. In production the store is containerd's content store (namespace "moby"), so a
// pushed image is immediately a docker image and every layer is on disk once.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"regexp"
)

var (
	ErrUnknown       = errors.New("unknown")
	ErrDigestInvalid = errors.New("digest does not match content")
)

type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Store keeps blobs (content-addressed, shared by all projects) and tags.
type Store interface {
	// Stat returns the size of a committed blob or ErrUnknown.
	Stat(ctx context.Context, digest string) (int64, error)
	Reader(ctx context.Context, digest string) (io.ReadCloser, int64, error)
	// Writer creates or resumes the upload id.
	Writer(ctx context.Context, id string) (Upload, error)
	Abort(ctx context.Context, id string) error
	// PutManifest stores a manifest blob that references refs, and points repo:tag at it
	// when tag is not empty.
	PutManifest(ctx context.Context, repo, tag string, desc Descriptor, body []byte, refs []Descriptor) error
	// Resolve returns the manifest tag points to.
	Resolve(ctx context.Context, repo, tag string) (Descriptor, error)
	Tags(ctx context.Context, repo string) ([]string, error)
}

type Upload interface {
	io.Writer
	// Offset is the number of bytes received so far.
	Offset() int64
	// Commit verifies the digest and makes the blob visible.
	Commit(ctx context.Context, digest string) error
	// Close releases the writer; the upload can be resumed with Store.Writer.
	Close() error
}

// Ownership limits which blobs a project sees. Blobs are stored once, but HEAD/GET answer
// only for blobs the project uploaded or referenced, so one project cannot read another's
// layers by digest.
type Ownership interface {
	Owns(project, digest string) (bool, error)
	Own(project string, digests ...string) error
}

var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func ValidDigest(d string) bool { return digestRe.MatchString(d) }

// digester tracks sha256 of written data.
type digester struct {
	h hash.Hash
}

func newDigester() *digester { return &digester{h: sha256.New()} }

func (d *digester) Write(p []byte) (int, error) { return d.h.Write(p) }

func (d *digester) Digest() string { return "sha256:" + hex.EncodeToString(d.h.Sum(nil)) }

func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}
