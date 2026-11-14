package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// v030Token is the token record as v0.3.0 wrote and reads it.
type v030Token struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"`
	Project string    `json:"project,omitempty"`
	Env     string    `json:"env,omitempty"`
	Admin   bool      `json:"admin,omitempty"`
	Name    string    `json:"name,omitempty"`
	Created time.Time `json:"created"`
}

// TestOpensV030Database writes a database the way v0.3.0 did (its five buckets, nothing of
// the npm registry), opens it with this binary, uses the registry, and reads it back the way
// v0.3.0 would: the old records are untouched and the new buckets are invisible to it.
func TestOpensV030Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := v030Token{ID: "abc123", Hash: "h", Admin: true, Name: "bootstrap", Created: time.Unix(1700000000, 0).UTC()}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{"projects", "tokens", "services", "releases", "blobs"} {
			if _, err := tx.CreateBucket([]byte(b)); err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(old)
		if err := tx.Bucket([]byte("tokens")).Put([]byte(old.ID), raw); err != nil {
			return err
		}
		return tx.Bucket([]byte("projects")).Put([]byte("shop"), []byte(`{"name":"shop","domains":["example.com"],"addons":null,"memory":0,"created":"2026-01-01T00:00:00Z"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := s.Token(old.ID); err != nil || !tok.Admin || tok.Name != "bootstrap" {
		t.Fatalf("old token: %+v %v", tok, err)
	}
	if p, err := s.Project("shop"); err != nil || p.Domains[0] != "example.com" {
		t.Fatalf("old project: %+v %v", p, err)
	}
	if err := s.PutNPMToken(NPMToken{ID: "n1", Name: "laptop", Scopes: []string{"npm:read:@a/*"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishNPM(NPMVersion{Name: "@a/b", Version: "1.0.0", SHA512: "x", Published: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNPMAudit(NPMAudit{Time: time.Now(), Action: "publish"}); err != nil {
		t.Fatal(err)
	}
	if s.TokenIDTaken("n1") != true || s.TokenIDTaken(old.ID) != true || s.TokenIDTaken("zzz") {
		t.Fatal("TokenIDTaken")
	}
	s.Close()

	// What v0.3.0 sees: its tokens bucket holds only its own records, in its own shape.
	db, err = bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bolt.Tx) error {
		n := 0
		tx.Bucket([]byte("tokens")).ForEach(func(k, v []byte) error {
			n++
			var tok v030Token
			dec := json.NewDecoder(bytes.NewReader(v))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&tok); err != nil || tok != old {
				t.Errorf("old token changed: %s %v", v, err)
			}
			return nil
		})
		if n != 1 {
			t.Errorf("the tokens bucket has %d records, want only the old one", n)
		}
		return nil
	})
}

func TestNPMRecords(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	v := NPMVersion{Name: "@a/b", Version: "1.0.0", SHA512: "aa", Published: now}
	if err := s.PublishNPM(v, []string{"beta"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishNPM(v, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("re-publish: %v", err)
	}
	p, err := s.NPMPackage("@a/b")
	if err != nil || p.DistTags["latest"] != "1.0.0" || p.DistTags["beta"] != "1.0.0" {
		t.Fatalf("%+v %v", p, err)
	}
	// "@a/b" must not list the versions of "@a/bc".
	if err := s.PublishNPM(NPMVersion{Name: "@a/bc", Version: "2.0.0", SHA512: "bb", Published: now}, nil); err != nil {
		t.Fatal(err)
	}
	if vs, _ := s.NPMVersions("@a/b"); len(vs) != 1 {
		t.Fatalf("prefix leak: %+v", vs)
	}
	if used, _ := s.NPMHashInUse("aa"); !used {
		t.Fatal("hash in use")
	}
	err = s.UpdateNPM("@a/b", func(p *NPMPackage, vs []NPMVersion) ([]NPMVersion, error) {
		vs[0].Removed = true
		return vs, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if used, _ := s.NPMHashInUse("aa"); used {
		t.Fatal("a removed version still holds its hash")
	}

	for i := 0; i < 10; i++ {
		s.AddNPMAudit(NPMAudit{Time: now.Add(time.Duration(i) * time.Hour), Action: "read", Package: "@a/b"})
	}
	es, _ := s.NPMAuditLog(3, nil)
	if len(es) != 3 || !es[0].Time.After(es[1].Time) {
		t.Fatalf("newest first: %+v", es)
	}
	if n, _ := s.PruneNPMAudit(now.Add(2*time.Hour), 100); n != 2 {
		t.Fatalf("pruned by age: %d", n)
	}
	if n, _ := s.PruneNPMAudit(time.Time{}, 5); n != 3 {
		t.Fatalf("pruned by count: %d", n)
	}
	if es, _ := s.NPMAuditLog(100, nil); len(es) != 5 || es[4].Time != now.Add(5*time.Hour) {
		t.Fatalf("kept: %+v", es)
	}
}
