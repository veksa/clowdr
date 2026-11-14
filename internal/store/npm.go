package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The npm registry keeps its records in buckets of their own, so a database written by this
// binary still opens in an older one (it does not know these buckets and never reads them),
// and a database of an older one opens here (Open creates the buckets).
var (
	bNPMTokens   = []byte("npm_tokens")   // key: token id
	bNPMPackages = []byte("npm_packages") // key: @scope/name
	bNPMVersions = []byte("npm_versions") // key: @scope/name/<version>
	bNPMAudit    = []byte("npm_audit")    // key: <seq big-endian>
)

// ErrExists is returned when a record that must not be overwritten already exists.
var ErrExists = errors.New("already exists")

// NPMToken is a token for the npm registry (/npm/ and /plugins/). It lives apart from the
// admin and project tokens, so it is unknown to every other API route.
type NPMToken struct {
	ID   string `json:"id"`
	Hash string `json:"hash,omitempty"` // sha256 hex of the secret
	// Name identifies the token for people: revoke and rotate take it. During a rotation's
	// grace period the old and the new token share it.
	Name string `json:"name"`
	// Scopes are "npm:read:<pattern>", "npm:publish:<pattern>" and "plugins:read"; a pattern
	// is "@scope/name" or "@scope/*".
	Scopes   []string  `json:"scopes"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	LastUsed time.Time `json:"last_used,omitzero"`
	// RotatedFrom is the id of the token this one replaced; ReplacedBy is set on the old token,
	// whose Expires is cut to the end of the grace period.
	RotatedFrom string `json:"rotated_from,omitempty"`
	ReplacedBy  string `json:"replaced_by,omitempty"`
}

// NPMPackage is a package's dist-tags; its versions are NPMVersion records.
type NPMPackage struct {
	Name     string            `json:"name"`
	DistTags map[string]string `json:"dist_tags"`
	Created  time.Time         `json:"created"`
	Modified time.Time         `json:"modified"`
}

// NPMVersion is one published version. A version deleted by retention stays as a tombstone
// (Removed set, no manifest), so the same version can never be published again.
type NPMVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Manifest is the version object from the publish body, without dist.tarball.
	Manifest json.RawMessage `json:"manifest,omitempty"`
	// SHA512 is the hex sha512 of the tarball, also its file name; Integrity and Shasum are
	// what npm puts in dist.
	SHA512      string    `json:"sha512"`
	Integrity   string    `json:"integrity"`
	Shasum      string    `json:"shasum"`
	Size        int64     `json:"size"`
	Published   time.Time `json:"published"`
	PublishedBy string    `json:"published_by,omitempty"`
	// RemoveAt is set by retention: the version is deprecated until then and deleted after.
	RemoveAt time.Time `json:"remove_at,omitzero"`
	Removed  bool      `json:"removed,omitempty"`
	// Plugin is set on a version that is a Claude Code plugin (its package.json says
	// "claude-plugin": true and the tarball passed the plugin checks).
	Plugin *NPMPlugin `json:"plugin,omitempty"`
	// PluginChecked says the server read the tarball for the plugin rules (every publish from
	// 0.5 on, or the first look at an older version): Plugin is then the whole answer. Without
	// it the version predates the check, and only its tarball can tell.
	PluginChecked bool `json:"plugin_checked,omitempty"`
}

// NPMPlugin is what the plugin marketplace lists for a version, and the zip it serves.
type NPMPlugin struct {
	// Name is the plugin's name: the package's unscoped name, equal to plugin.json's name.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ZipSHA256 and ZipSize describe npm/zips/<sha512 hex>.zip, built from the tarball. Empty
	// until the zip is built.
	ZipSHA256 string `json:"zip_sha256,omitempty"`
	ZipSize   int64  `json:"zip_size,omitempty"`
}

// NPMAudit is one entry of the registry's audit log.
type NPMAudit struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	Action  string    `json:"action"`
	Token   string    `json:"token,omitempty"` // token name
	TokenID string    `json:"token_id,omitempty"`
	Addr    string    `json:"addr,omitempty"`
	Package string    `json:"package,omitempty"`
	Version string    `json:"version,omitempty"`
	Tag     string    `json:"tag,omitempty"`
	Status  int       `json:"status,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// npm tokens

func (s *Store) PutNPMToken(t NPMToken) error {
	return s.db.Update(func(tx *bolt.Tx) error { return put(tx, bNPMTokens, t.ID, t) })
}

func (s *Store) NPMToken(id string) (t NPMToken, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		t, err = get[NPMToken](tx, bNPMTokens, id)
		return err
	})
	return
}

func (s *Store) NPMTokens() (ts []NPMToken, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		ts, err = list[NPMToken](tx, bNPMTokens, "")
		return err
	})
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Name != ts[j].Name {
			return ts[i].Name < ts[j].Name
		}
		return ts[i].Created.Before(ts[j].Created)
	})
	return
}

// UpdateNPMTokens runs fn with every npm token in one transaction; fn returns the tokens to
// write and the ids to delete.
func (s *Store) UpdateNPMTokens(fn func(ts []NPMToken) (put []NPMToken, del []string, err error)) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		ts, err := list[NPMToken](tx, bNPMTokens, "")
		if err != nil {
			return err
		}
		ps, ds, err := fn(ts)
		if err != nil {
			return err
		}
		for _, t := range ps {
			if err := put(tx, bNPMTokens, t.ID, t); err != nil {
				return err
			}
		}
		for _, id := range ds {
			if err := tx.Bucket(bNPMTokens).Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
}

// TouchNPMToken records the token's last use.
func (s *Store) TouchNPMToken(id string, at time.Time) error {
	return s.db.Batch(func(tx *bolt.Tx) error {
		t, err := get[NPMToken](tx, bNPMTokens, id)
		if err != nil {
			return err
		}
		t.LastUsed = at
		return put(tx, bNPMTokens, id, t)
	})
}

// TokenIDTaken reports whether id is used by any token, of any kind.
func (s *Store) TokenIDTaken(id string) bool {
	taken := false
	s.db.View(func(tx *bolt.Tx) error {
		taken = tx.Bucket(bTokens).Get([]byte(id)) != nil || tx.Bucket(bNPMTokens).Get([]byte(id)) != nil
		return nil
	})
	return taken
}

// npm packages

func versionKey(name, version string) string { return name + "/" + version }

func (s *Store) NPMPackage(name string) (p NPMPackage, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		p, err = get[NPMPackage](tx, bNPMPackages, name)
		return err
	})
	return
}

func (s *Store) NPMPackages() (ps []NPMPackage, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		ps, err = list[NPMPackage](tx, bNPMPackages, "")
		return err
	})
	return
}

// NPMVersions lists every version of a package, tombstones included.
func (s *Store) NPMVersions(name string) (vs []NPMVersion, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		vs, err = list[NPMVersion](tx, bNPMVersions, name+"/")
		return err
	})
	return
}

func (s *Store) NPMVersion(name, version string) (v NPMVersion, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v, err = get[NPMVersion](tx, bNPMVersions, versionKey(name, version))
		return err
	})
	return
}

// PublishNPM stores a new version and points tags at it, creating the package on its first
// version. A version that exists, or existed, returns ErrExists.
func (s *Store) PublishNPM(v NPMVersion, tags []string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bNPMVersions).Get([]byte(versionKey(v.Name, v.Version))) != nil {
			return ErrExists
		}
		p, err := get[NPMPackage](tx, bNPMPackages, v.Name)
		if errors.Is(err, ErrNotFound) {
			p = NPMPackage{Name: v.Name, Created: v.Published}
		} else if err != nil {
			return err
		}
		if p.DistTags == nil {
			p.DistTags = map[string]string{}
		}
		for _, t := range tags {
			p.DistTags[t] = v.Version
		}
		if p.DistTags["latest"] == "" {
			p.DistTags["latest"] = v.Version
		}
		p.Modified = v.Published
		if err := put(tx, bNPMVersions, versionKey(v.Name, v.Version), v); err != nil {
			return err
		}
		return put(tx, bNPMPackages, v.Name, p)
	})
}

// UpdateNPM runs fn on a package and its versions (tombstones included) in one transaction and
// writes back the package and every version fn changed (by value).
func (s *Store) UpdateNPM(name string, fn func(p *NPMPackage, vs []NPMVersion) ([]NPMVersion, error)) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, err := get[NPMPackage](tx, bNPMPackages, name)
		if err != nil {
			return err
		}
		vs, err := list[NPMVersion](tx, bNPMVersions, name+"/")
		if err != nil {
			return err
		}
		changed, err := fn(&p, vs)
		if err != nil {
			return err
		}
		for _, v := range changed {
			if err := put(tx, bNPMVersions, versionKey(v.Name, v.Version), v); err != nil {
				return err
			}
		}
		return put(tx, bNPMPackages, name, p)
	})
}

// NPMHashInUse reports whether a live (not removed) version has the tarball sha512.
func (s *Store) NPMHashInUse(sha512 string) (bool, error) {
	used := false
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bNPMVersions).ForEach(func(_, b []byte) error {
			var v NPMVersion
			if err := json.Unmarshal(b, &v); err != nil {
				return err
			}
			if !v.Removed && v.SHA512 == sha512 {
				used = true
			}
			return nil
		})
	})
	return used, err
}

// npm audit log

// AddNPMAudit appends entries. Writes are batched: a yarn install reads many packages at once.
func (s *Store) AddNPMAudit(es ...NPMAudit) error {
	return s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(bNPMAudit)
		for _, e := range es {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			e.Seq = seq
			if err := put(tx, bNPMAudit, seqKey(seq), e); err != nil {
				return err
			}
		}
		return nil
	})
}

func seqKey(n uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return string(b[:])
}

// NPMAuditLog returns up to limit entries matching keep, newest first.
func (s *Store) NPMAuditLog(limit int, keep func(NPMAudit) bool) ([]NPMAudit, error) {
	var out []NPMAudit
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bNPMAudit).Cursor()
		for k, v := c.Last(); k != nil && len(out) < limit; k, v = c.Prev() {
			var e NPMAudit
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if keep == nil || keep(e) {
				out = append(out, e)
			}
		}
		return nil
	})
	return out, err
}

// PruneNPMAudit deletes entries older than before and the oldest beyond max.
func (s *Store) PruneNPMAudit(before time.Time, max int) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bNPMAudit)
		over := b.Stats().KeyN - max
		var keys [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if over > len(keys) {
				keys = append(keys, append([]byte(nil), k...))
				continue
			}
			var e NPMAudit
			if err := json.Unmarshal(v, &e); err != nil || !e.Time.Before(before) {
				break
			}
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		n = len(keys)
		return nil
	})
	return n, err
}
