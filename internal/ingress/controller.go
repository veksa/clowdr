package ingress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Nginx runs commands in the nginx container.
type Nginx interface {
	// Test runs `nginx -t` and returns its output.
	Test(ctx context.Context) (string, error)
	// Reload runs `nginx -s reload` (graceful).
	Reload(ctx context.Context) error
}

// Controller writes config files and reloads nginx. Files are only kept when `nginx -t`
// accepts them; otherwise the previous content is restored and nginx keeps running on it.
type Controller struct {
	Dir   string // ConfDir on the host; tests use a temp dir
	Nginx Nginx

	mu sync.Mutex
}

// File is one config file relative to Dir.
type File struct {
	Path    string // e.g. "conf.d/shop.conf"
	Content string // "" removes the file
}

var ErrInvalidConfig = errors.New("nginx rejected the config")

// ErrNginxUnreachable marks a Test or Reload whose command could not run at all (no container,
// exec refused, Docker not answering): nothing is known about the config then.
var ErrNginxUnreachable = errors.New("cannot run nginx")

// Apply writes files, checks the whole config and reloads. Unchanged files are skipped;
// when nothing changed nginx is not reloaded.
func (c *Controller) Apply(ctx context.Context, files ...File) error {
	return c.apply(ctx, false, files)
}

// ApplyAndReload is Apply that also tests and reloads when no file changed, so nginx is
// brought to what is on disk whatever it loaded before (clowdrd start).
func (c *Controller) ApplyAndReload(ctx context.Context, files ...File) error {
	return c.apply(ctx, true, files)
}

func (c *Controller) apply(ctx context.Context, force bool, files []File) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	type backup struct {
		path    string
		old     []byte
		existed bool
	}
	var changed []backup
	restore := func() {
		for _, b := range changed {
			if b.existed {
				_ = writeAtomic(b.path, b.old)
			} else {
				_ = os.Remove(b.path)
			}
		}
	}
	for _, f := range files {
		p := filepath.Join(c.Dir, filepath.FromSlash(f.Path))
		old, err := os.ReadFile(p)
		existed := err == nil
		if err != nil && !os.IsNotExist(err) {
			restore()
			return err
		}
		if f.Content == "" {
			if !existed {
				continue
			}
			if err := os.Remove(p); err != nil {
				restore()
				return err
			}
		} else {
			if existed && bytes.Equal(old, []byte(f.Content)) {
				continue
			}
			if err := writeAtomic(p, []byte(f.Content)); err != nil {
				restore()
				return err
			}
		}
		changed = append(changed, backup{p, old, existed})
	}
	if (len(changed) == 0 && !force) || c.Nginx == nil {
		return nil
	}
	if out, err := c.Nginx.Test(ctx); err != nil {
		// Untested files are not kept either: the contract is that only checked files stay.
		restore()
		if errors.Is(err, ErrNginxUnreachable) {
			return fmt.Errorf("nginx -t: %w", err)
		}
		return fmt.Errorf("%w: %v\n%s", ErrInvalidConfig, err, out)
	}
	if err := c.Nginx.Reload(ctx); err != nil {
		return fmt.Errorf("nginx reload: %w", err)
	}
	return nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ProjectFile is the conf.d path of a project.
func ProjectFile(project string) string { return "conf.d/" + project + ".conf" }

// Write writes files without testing or reloading; used before nginx runs for the first time.
func (c *Controller) Write(files ...File) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range files {
		p := filepath.Join(c.Dir, filepath.FromSlash(f.Path))
		if f.Content == "" {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := writeAtomic(p, []byte(f.Content)); err != nil {
			return err
		}
	}
	return nil
}
