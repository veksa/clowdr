// Package store keeps clowdrd state in a bbolt file: projects, token hashes, services
// (with the last applied spec) and releases.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/veksa/clowdr/internal/spec"
)

var ErrNotFound = errors.New("not found")

var (
	bProjects = []byte("projects")
	bTokens   = []byte("tokens")
	bServices = []byte("services") // key: project/service
	bReleases = []byte("releases") // key: project/service/<seq big-endian>
)

type Project struct {
	Name    string    `json:"name"`
	Domains []string  `json:"domains"` // allowed domain patterns, "*.x.com" covers one level
	Addons  []string  `json:"addons"`  // addons the project may run
	Memory  int64     `json:"memory"`  // quota in bytes, 0 = unlimited
	Created time.Time `json:"created"`
	Seeded  bool      `json:"seeded,omitempty"` // from platform.yml
	// NginxExtra allows raw nginx snippets in the project's deploy.yml (trusted projects only).
	NginxExtra bool `json:"nginx_extra,omitempty"`
	// RenamedFrom is the old name while containers, network and images of a rename are being
	// removed; Recover finishes the job after a restart.
	RenamedFrom string `json:"renamed_from,omitempty"`
}

type Token struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"` // sha256 hex of the secret
	Project string    `json:"project,omitempty"`
	Env     string    `json:"env,omitempty"`
	Admin   bool      `json:"admin,omitempty"`
	Name    string    `json:"name,omitempty"`
	Created time.Time `json:"created"`
}

type Service struct {
	Project  string            `json:"project"`
	Name     string            `json:"name"`
	Spec     spec.Service      `json:"spec"`
	Current  uint64            `json:"current,omitempty"`  // active release
	Previous uint64            `json:"previous,omitempty"` // kept for rollback
	Env      map[string]string `json:"env,omitempty"`      // values set with `clowctl env set`
	Stopped  bool              `json:"stopped,omitempty"`
	Updated  time.Time         `json:"updated"`
}

type ReleaseStatus string

const (
	StatusDeploying  ReleaseStatus = "deploying"
	StatusActive     ReleaseStatus = "active"
	StatusFailed     ReleaseStatus = "failed"
	StatusSuperseded ReleaseStatus = "superseded"
	StatusRemoved    ReleaseStatus = "removed" // image garbage-collected
)

type Release struct {
	ID         uint64        `json:"id"`
	Project    string        `json:"project"`
	Service    string        `json:"service"`
	Image      string        `json:"image"` // reference the container runs, e.g. clowdr/shop/server@sha256:...
	Digest     string        `json:"digest,omitempty"`
	Version    string        `json:"version,omitempty"` // git sha or tag from the CLI
	Author     string        `json:"author,omitempty"`
	Status     ReleaseStatus `json:"status"`
	Error      string        `json:"error,omitempty"`
	Containers []string      `json:"containers,omitempty"`
	Spec       spec.Service  `json:"spec"`
	Rollback   bool          `json:"rollback,omitempty"`
	// AllowOutdated is set when the deploy skipped the check of locked private packages
	// (clowctl deploy --allow-outdated).
	AllowOutdated bool `json:"allow_outdated,omitempty"`
	// ImageRemoved is set when GC deleted the image; a failed release keeps its status.
	ImageRemoved bool      `json:"image_removed,omitempty"`
	Created      time.Time `json:"created"`
	Finished     time.Time `json:"finished,omitzero"`
}

type Store struct {
	db *bolt.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bProjects, bTokens, bServices, bReleases, bBlobs, bNPMTokens, bNPMPackages, bNPMVersions, bNPMAudit, bNPMClaims, bMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return backfillClaims(tx)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func get[T any](tx *bolt.Tx, bucket []byte, key string) (T, error) {
	var v T
	b := tx.Bucket(bucket).Get([]byte(key))
	if b == nil {
		return v, ErrNotFound
	}
	return v, json.Unmarshal(b, &v)
}

func put(tx *bolt.Tx, bucket []byte, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), b)
}

func list[T any](tx *bolt.Tx, bucket []byte, prefix string) ([]T, error) {
	var out []T
	c := tx.Bucket(bucket).Cursor()
	for k, v := c.Seek([]byte(prefix)); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
		var t T
		if err := json.Unmarshal(v, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func hasPrefix(k []byte, p string) bool {
	return len(k) >= len(p) && string(k[:len(p)]) == p
}

// Projects

func (s *Store) Project(name string) (p Project, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		p, err = get[Project](tx, bProjects, name)
		return err
	})
	return
}

func (s *Store) Projects() (ps []Project, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		ps, err = list[Project](tx, bProjects, "")
		return err
	})
	return
}

func (s *Store) PutProject(p Project) error {
	if p.Created.IsZero() {
		p.Created = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bProjects, p.Name, p) })
}

// DeleteProject removes the project, its services, releases and tokens.
func (s *Store) DeleteProject(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bProjects).Delete([]byte(name)); err != nil {
			return err
		}
		for _, b := range [][]byte{bServices, bReleases, bBlobs} {
			if err := deletePrefix(tx.Bucket(b), name+"/"); err != nil {
				return err
			}
		}
		toks, err := list[Token](tx, bTokens, "")
		if err != nil {
			return err
		}
		for _, t := range toks {
			if t.Project == name {
				if err := tx.Bucket(bTokens).Delete([]byte(t.ID)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func deletePrefix(b *bolt.Bucket, prefix string) error {
	var keys [][]byte
	c := b.Cursor()
	for k, _ := c.Seek([]byte(prefix)); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
		keys = append(keys, append([]byte(nil), k...))
	}
	for _, k := range keys {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// Tokens

func (s *Store) PutToken(t Token) error {
	if t.Created.IsZero() {
		t.Created = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bTokens, t.ID, t) })
}

func (s *Store) Token(id string) (t Token, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		t, err = get[Token](tx, bTokens, id)
		return err
	})
	return
}

func (s *Store) Tokens() (ts []Token, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		ts, err = list[Token](tx, bTokens, "")
		return err
	})
	return
}

func (s *Store) DeleteToken(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bTokens).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return tx.Bucket(bTokens).Delete([]byte(id))
	})
}

// Services

func svcKey(project, name string) string { return project + "/" + name }

func (s *Store) Service(project, name string) (v Service, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v, err = get[Service](tx, bServices, svcKey(project, name))
		return err
	})
	return
}

// Services lists services of a project, or of all projects when project is "".
func (s *Store) Services(project string) (vs []Service, err error) {
	prefix := ""
	if project != "" {
		prefix = project + "/"
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		vs, err = list[Service](tx, bServices, prefix)
		return err
	})
	return
}

func (s *Store) PutService(v Service) error {
	v.Updated = time.Now().UTC()
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bServices, svcKey(v.Project, v.Name), v) })
}

// UpdateService applies fn to the stored service atomically. fn gets ErrNotFound-free zero
// value with Project/Name set when the service does not exist yet.
func (s *Store) UpdateService(project, name string, fn func(*Service) error) (Service, error) {
	var out Service
	err := s.db.Update(func(tx *bolt.Tx) error {
		v, err := get[Service](tx, bServices, svcKey(project, name))
		if errors.Is(err, ErrNotFound) {
			v = Service{Project: project, Name: name}
		} else if err != nil {
			return err
		}
		if err := fn(&v); err != nil {
			return err
		}
		v.Updated = time.Now().UTC()
		out = v
		return put(tx, bServices, svcKey(project, name), v)
	})
	return out, err
}

func (s *Store) DeleteService(project, name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bServices).Delete([]byte(svcKey(project, name))); err != nil {
			return err
		}
		return deletePrefix(tx.Bucket(bReleases), svcKey(project, name)+"/")
	})
}

// Releases

func relKey(project, service string, id uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return svcKey(project, service) + "/" + string(b[:])
}

// CreateRelease assigns the next ID and stores r.
func (s *Store) CreateRelease(r *Release) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		id, err := tx.Bucket(bReleases).NextSequence()
		if err != nil {
			return err
		}
		r.ID = id
		if r.Created.IsZero() {
			r.Created = time.Now().UTC()
		}
		return put(tx, bReleases, relKey(r.Project, r.Service, r.ID), r)
	})
}

func (s *Store) PutRelease(r Release) error {
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bReleases, relKey(r.Project, r.Service, r.ID), r) })
}

func (s *Store) Release(project, service string, id uint64) (r Release, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		r, err = get[Release](tx, bReleases, relKey(project, service, id))
		return err
	})
	return
}

// Releases returns releases of a service, newest first.
func (s *Store) Releases(project, service string) (rs []Release, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		rs, err = list[Release](tx, bReleases, svcKey(project, service)+"/")
		return err
	})
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID > rs[j].ID })
	return
}

// Blob ownership (registry): which project uploaded or referenced which blob.

var bBlobs = []byte("blobs") // key: project/digest

func (s *Store) Owns(project, digest string) (bool, error) {
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bBlobs).Get([]byte(project+"/"+digest)) != nil
		return nil
	})
	return ok, err
}

func (s *Store) Own(project string, digests ...string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, d := range digests {
			if err := tx.Bucket(bBlobs).Put([]byte(project+"/"+d), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteRelease(project, service string, id uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bReleases).Delete([]byte(relKey(project, service, id))) })
}
