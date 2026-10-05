package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImagePrefix is the registry host part of every image clowdrd creates in docker, so
// `clowdr.local/shop/server:3f2a1c` is the same image whatever host the CLI pushed to.
const ImagePrefix = "clowdr.local/"

// ContainerdStore writes straight into containerd's content store in docker's namespace.
// Blobs of an unfinished push are held by a lease that expires after UploadTTL, so an
// interrupted `docker push` is cleaned up by containerd's GC.
type ContainerdStore struct {
	Client      *containerd.Client
	Namespace   string // "moby"
	Snapshotter string // docker's snapshotter, "overlayfs"
	UploadTTL   time.Duration

	mu    sync.Mutex
	lease string
	until time.Time
}

func NewContainerdStore(address, snapshotter string) (*ContainerdStore, error) {
	c, err := containerd.New(address, containerd.WithDefaultNamespace("moby"))
	if err != nil {
		return nil, fmt.Errorf("containerd %s: %w", address, err)
	}
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	return &ContainerdStore{Client: c, Namespace: "moby", Snapshotter: snapshotter, UploadTTL: time.Hour}, nil
}

func (s *ContainerdStore) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, s.Namespace)
}

// leased returns ctx bound to a lease that protects fresh blobs until a manifest references
// them. One lease is shared for UploadTTL/2, so each blob is protected for at least that long.
func (s *ContainerdStore) leased(ctx context.Context) (context.Context, error) {
	ctx = s.ctx(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease == "" || time.Now().After(s.until) {
		l, err := s.Client.LeasesService().Create(ctx, leases.WithRandomID(), leases.WithExpiration(s.UploadTTL),
			leases.WithLabel("clowdr", "upload"))
		if err != nil {
			return nil, err
		}
		s.lease, s.until = l.ID, time.Now().Add(s.UploadTTL/2)
	}
	return leases.WithLease(ctx, s.lease), nil
}

func (s *ContainerdStore) Stat(ctx context.Context, d string) (int64, error) {
	if !ValidDigest(d) {
		return 0, ErrUnknown
	}
	info, err := s.Client.ContentStore().Info(s.ctx(ctx), digest.Digest(d))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return 0, ErrUnknown
		}
		return 0, err
	}
	return info.Size, nil
}

func (s *ContainerdStore) Reader(ctx context.Context, d string) (io.ReadCloser, int64, error) {
	size, err := s.Stat(ctx, d)
	if err != nil {
		return nil, 0, err
	}
	ra, err := s.Client.ContentStore().ReaderAt(s.ctx(ctx), ocispec.Descriptor{Digest: digest.Digest(d), Size: size})
	if err != nil {
		return nil, 0, err
	}
	return readCloser{content.NewReader(ra), ra}, size, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func ingestRef(id string) string { return "clowdr-upload-" + id }

type cdUpload struct {
	w   content.Writer
	n   int64
	ctx context.Context
}

func (s *ContainerdStore) Writer(ctx context.Context, id string) (Upload, error) {
	// The writer outlives a cancelled request: it is closed explicitly and can be resumed.
	wctx, err := s.leased(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	w, err := content.OpenWriter(wctx, s.Client.ContentStore(), content.WithRef(ingestRef(id)))
	if err != nil {
		return nil, err
	}
	st, err := w.Status()
	if err != nil {
		w.Close()
		return nil, err
	}
	return &cdUpload{w: w, n: st.Offset, ctx: wctx}, nil
}

func (u *cdUpload) Write(p []byte) (int, error) {
	n, err := u.w.Write(p)
	u.n += int64(n)
	return n, err
}

func (u *cdUpload) Offset() int64 { return u.n }

func (u *cdUpload) Commit(ctx context.Context, d string) error {
	err := u.w.Commit(u.ctx, u.n, digest.Digest(d))
	switch {
	case err == nil, errdefs.IsAlreadyExists(err):
		return nil
	case errdefs.IsFailedPrecondition(err) || errdefs.IsInvalidArgument(err):
		return fmt.Errorf("%w: %v", ErrDigestInvalid, err)
	}
	return err
}

func (u *cdUpload) Close() error { return u.w.Close() }

func (s *ContainerdStore) Abort(ctx context.Context, id string) error {
	err := s.Client.ContentStore().Abort(s.ctx(ctx), ingestRef(id))
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// PutManifest writes the manifest with gc references to its config, layers and children, then
// creates or updates the docker image and unpacks it, so `docker run` needs no pull.
func (s *ContainerdStore) PutManifest(ctx context.Context, repo, tag string, desc Descriptor, body []byte, refs []Descriptor) error {
	lctx, err := s.leased(ctx)
	if err != nil {
		return err
	}
	var m manifestDoc
	if err := json.Unmarshal(body, &m); err != nil {
		return err
	}
	labels := map[string]string{}
	if m.Config != nil {
		labels["containerd.io/gc.ref.content.config"] = m.Config.Digest
	}
	for i, l := range m.Layers {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = l.Digest
	}
	for i, c := range m.Manifests {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", i)] = c.Digest
	}
	od := ocispec.Descriptor{MediaType: desc.MediaType, Digest: digest.Digest(desc.Digest), Size: desc.Size}
	cs := s.Client.ContentStore()
	err = content.WriteBlob(lctx, cs, "clowdr-manifest-"+desc.Digest, strings.NewReader(string(body)), od, content.WithLabels(labels))
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("write manifest: %w", err)
	}
	if errdefs.IsAlreadyExists(err) {
		// Make sure the gc references are there even if the blob came from elsewhere.
		info, ierr := cs.Info(lctx, od.Digest)
		if ierr == nil {
			var fields []string
			if info.Labels == nil {
				info.Labels = map[string]string{}
			}
			for k, v := range labels {
				info.Labels[k] = v
				fields = append(fields, "labels."+k)
			}
			if len(fields) > 0 {
				cs.Update(lctx, info, fields...)
			}
		}
	}
	if tag == "" {
		return nil
	}
	img := images.Image{
		Name:   ImagePrefix + repo + ":" + tag,
		Target: od,
		Labels: map[string]string{"clowdr": "1"},
	}
	is := s.Client.ImageService()
	if _, err := is.Create(lctx, img); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("create image: %w", err)
		}
		if _, err := is.Update(lctx, img, "target", "labels.clowdr"); err != nil {
			return fmt.Errorf("update image: %w", err)
		}
	}
	ci := containerd.NewImageWithPlatform(s.Client, img, platforms.Default())
	if err := ci.Unpack(lctx, s.Snapshotter); err != nil {
		return fmt.Errorf("unpack %s: %w", img.Name, err)
	}
	return nil
}

func (s *ContainerdStore) Resolve(ctx context.Context, repo, tag string) (Descriptor, error) {
	img, err := s.Client.ImageService().Get(s.ctx(ctx), ImagePrefix+repo+":"+tag)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Descriptor{}, ErrUnknown
		}
		return Descriptor{}, err
	}
	return Descriptor{MediaType: img.Target.MediaType, Digest: img.Target.Digest.String(), Size: img.Target.Size}, nil
}

func (s *ContainerdStore) Tags(ctx context.Context, repo string) ([]string, error) {
	prefix := ImagePrefix + repo + ":"
	imgs, err := s.Client.ImageService().List(s.ctx(ctx), fmt.Sprintf(`name~="^%s"`, strings.ReplaceAll(prefix, ".", `\\.`)))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, i := range imgs {
		if t, ok := strings.CutPrefix(i.Name, prefix); ok {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Close releases the client.
func (s *ContainerdStore) Close() error {
	if s.Client == nil {
		return nil
	}
	return s.Client.Close()
}
