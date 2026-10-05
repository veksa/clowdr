package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FSStore keeps blobs in a directory. Used in tests and for local development on machines
// without the containerd image store.
type FSStore struct {
	Dir string
	mu  sync.Mutex
}

func (s *FSStore) blobPath(d string) string {
	hex := strings.TrimPrefix(d, "sha256:")
	return filepath.Join(s.Dir, "blobs", "sha256", hex[:2], hex)
}

func (s *FSStore) uploadPath(id string) string { return filepath.Join(s.Dir, "uploads", id) }

func (s *FSStore) tagPath(repo, tag string) string {
	return filepath.Join(s.Dir, "repos", filepath.FromSlash(repo), "tags", tag)
}

func (s *FSStore) Stat(_ context.Context, d string) (int64, error) {
	if !ValidDigest(d) {
		return 0, ErrUnknown
	}
	fi, err := os.Stat(s.blobPath(d))
	if err != nil {
		return 0, ErrUnknown
	}
	return fi.Size(), nil
}

func (s *FSStore) Reader(_ context.Context, d string) (io.ReadCloser, int64, error) {
	if !ValidDigest(d) {
		return nil, 0, ErrUnknown
	}
	f, err := os.Open(s.blobPath(d))
	if err != nil {
		return nil, 0, ErrUnknown
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

type fsUpload struct {
	s    *FSStore
	id   string
	f    *os.File
	dg   *digester
	size int64
}

func (s *FSStore) Writer(_ context.Context, id string) (Upload, error) {
	p := s.uploadPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	dg := newDigester()
	n, err := io.Copy(dg, f) // resume: rehash what is already there
	if err != nil {
		f.Close()
		return nil, err
	}
	return &fsUpload{s: s, id: id, f: f, dg: dg, size: n}, nil
}

func (u *fsUpload) Write(p []byte) (int, error) {
	n, err := u.f.Write(p)
	u.dg.Write(p[:n])
	u.size += int64(n)
	return n, err
}

func (u *fsUpload) Offset() int64 { return u.size }

func (u *fsUpload) Commit(_ context.Context, d string) error {
	if u.dg.Digest() != d {
		return ErrDigestInvalid
	}
	if err := u.f.Close(); err != nil {
		return err
	}
	dst := u.s.blobPath(d)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(u.s.uploadPath(u.id), dst)
}

func (u *fsUpload) Close() error {
	err := u.f.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func (s *FSStore) Abort(_ context.Context, id string) error {
	err := os.Remove(s.uploadPath(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *FSStore) PutManifest(ctx context.Context, repo, tag string, desc Descriptor, body []byte, _ []Descriptor) error {
	if _, err := s.Stat(ctx, desc.Digest); err != nil {
		w, err := s.Writer(ctx, "manifest-"+strings.TrimPrefix(desc.Digest, "sha256:"))
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			w.Close()
			return err
		}
		if err := w.Commit(ctx, desc.Digest); err != nil {
			w.Close()
			return err
		}
	}
	if tag == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.tagPath(repo, tag)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(desc)
	return os.WriteFile(p, b, 0o644)
}

func (s *FSStore) Resolve(_ context.Context, repo, tag string) (Descriptor, error) {
	var d Descriptor
	b, err := os.ReadFile(s.tagPath(repo, tag))
	if err != nil {
		return d, ErrUnknown
	}
	return d, json.Unmarshal(b, &d)
}

func (s *FSStore) Tags(_ context.Context, repo string) ([]string, error) {
	es, err := os.ReadDir(filepath.Dir(s.tagPath(repo, "x")))
	if err != nil {
		return nil, nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
