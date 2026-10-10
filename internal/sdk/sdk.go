// Package sdk serves the client packages embedded in clowdrd: every clowdr server hands out
// the same versions, so a project keeps installing them after moving to another server.
// The tarballs are built by sdk/<lang>/pack.sh into files/.
package sdk

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed files
var files embed.FS

// Package names one tarball: clowdr-node-0.1.0.tgz.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	File    string `json:"file"`
}

// Index lists the embedded packages of one language, newest version last.
type Index struct {
	Latest   string    `json:"latest"`
	Packages []Package `json:"packages"`
	Readme   string    `json:"readme"`
}

// NodeIndex lists the clowdr-node tarballs.
func NodeIndex() Index {
	es, _ := fs.ReadDir(files, "files")
	var ps []Package
	for _, e := range es {
		v, ok := strings.CutPrefix(e.Name(), "clowdr-node-")
		if !ok || !strings.HasSuffix(v, ".tgz") {
			continue
		}
		ps = append(ps, Package{Name: "clowdr-node", Version: strings.TrimSuffix(v, ".tgz"), File: e.Name()})
	}
	sort.Slice(ps, func(i, j int) bool { return lessVersion(ps[i].Version, ps[j].Version) })
	idx := Index{Packages: ps, Readme: "README.md"}
	if len(ps) > 0 {
		idx.Latest = ps[len(ps)-1].Version
	}
	return idx
}

// File returns an embedded file by name ("clowdr-node-0.1.0.tgz", "README.md").
func File(name string) ([]byte, error) {
	if name != path.Base(name) {
		return nil, fs.ErrNotExist
	}
	return files.ReadFile("files/" + name)
}

// Handler serves GET /sdk/node/ (the index), /sdk/node/<tarball> and /sdk/node/README.md.
// The packages are open source: no authentication.
func Handler(writeJSON func(http.ResponseWriter, int, any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/sdk/node/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == "" {
			writeJSON(w, 200, NodeIndex())
			return
		}
		b, err := File(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(name, ".tgz"):
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // a version never changes
		case strings.HasSuffix(name, ".md"):
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		}
		w.Write(b)
	})
}

// lessVersion compares dotted numeric versions ("0.10.0" after "0.9.1").
func lessVersion(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
