package store

import (
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/veksa/clowdr/internal/spec"
)

// RenameProject moves a project with its services, releases, blob ownership and tokens to a new
// name in one transaction, and rewrites the name in expose_to and accept_from of other projects.
// Tokens stay valid. fn adjusts each of the project's releases (images, containers). The project
// is left marked RenamedFrom until FinishRename: what carries the old name is still being removed.
func (s *Store) RenameProject(old, name string, fn func(*Release)) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, err := get[Project](tx, bProjects, old)
		if err != nil {
			return err
		}
		if tx.Bucket(bProjects).Get([]byte(name)) != nil {
			return fmt.Errorf("project %s already exists", name)
		}
		if err := tx.Bucket(bProjects).Delete([]byte(old)); err != nil {
			return err
		}
		p.Name, p.RenamedFrom = name, old
		if err := put(tx, bProjects, name, p); err != nil {
			return err
		}

		// The whole buckets, not a prefix: other projects name this one in their specs. Key order
		// does not matter, as nothing here is read back through a cursor after a write.
		svcs, err := list[Service](tx, bServices, "")
		if err != nil {
			return err
		}
		for _, v := range svcs {
			renameInSpec(&v.Spec, old, name)
			if v.Project == old {
				if err := tx.Bucket(bServices).Delete([]byte(svcKey(old, v.Name))); err != nil {
					return err
				}
				v.Project = name
			}
			if err := put(tx, bServices, svcKey(v.Project, v.Name), v); err != nil {
				return err
			}
		}
		rels, err := list[Release](tx, bReleases, "")
		if err != nil {
			return err
		}
		for _, r := range rels {
			renameInSpec(&r.Spec, old, name)
			if r.Project == old {
				if err := tx.Bucket(bReleases).Delete([]byte(relKey(old, r.Service, r.ID))); err != nil {
					return err
				}
				r.Project = name
				if fn != nil {
					fn(&r)
				}
			}
			if err := put(tx, bReleases, relKey(r.Project, r.Service, r.ID), r); err != nil {
				return err
			}
		}

		var digests []string
		blobs := tx.Bucket(bBlobs)
		c := blobs.Cursor()
		for k, _ := c.Seek([]byte(old + "/")); k != nil && hasPrefix(k, old+"/"); k, _ = c.Next() {
			digests = append(digests, string(k[len(old)+1:]))
		}
		for _, d := range digests {
			if err := blobs.Delete([]byte(old + "/" + d)); err != nil {
				return err
			}
			if err := blobs.Put([]byte(name+"/"+d), []byte{1}); err != nil {
				return err
			}
		}

		toks, err := list[Token](tx, bTokens, "")
		if err != nil {
			return err
		}
		for _, t := range toks {
			if t.Project == old {
				t.Project = name
				if err := put(tx, bTokens, t.ID, t); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// FinishRename clears the mark RenameProject left.
func (s *Store) FinishRename(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, err := get[Project](tx, bProjects, name)
		if err != nil {
			return err
		}
		p.RenamedFrom = ""
		return put(tx, bProjects, name, p)
	})
}

func renameInSpec(s *spec.Service, old, name string) {
	for _, l := range [][]string{s.ExposeTo, s.AcceptFrom} {
		for i, v := range l {
			if v == old {
				l[i] = name
			}
		}
	}
}
