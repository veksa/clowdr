package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lockedPackage is one package version a lockfile pins.
type lockedPackage struct {
	Name    string
	Version string
	File    string // the lockfile it came from
}

// parseYarnLock reads a yarn 2+ (berry) lockfile: every top-level entry has a "resolution" like
// "@scope/name@npm:1.2.3". Entries of other protocols (workspace:, patch:, git, file) are not
// from a registry and are skipped.
func parseYarnLock(path string) ([]lockedPackage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []lockedPackage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "  resolution: ") {
			continue
		}
		res := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "  resolution: ")), `"`)
		i := strings.LastIndex(res, "@npm:")
		if i <= 0 {
			continue
		}
		name, version := res[:i], res[i+len("@npm:"):]
		if strings.Contains(name, ":") || version == "" || strings.ContainsAny(version, "#:") {
			continue // e.g. patch:... resolutions carry a nested reference
		}
		out = append(out, lockedPackage{Name: name, Version: version, File: path})
	}
	return out, sc.Err()
}

// parsePackageLock reads package-lock.json (lockfile v2 and v3 "packages", v1 "dependencies").
func parsePackageLock(path string) ([]lockedPackage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Version  string `json:"version"`
			Link     bool   `json:"link"`
			Resolved string `json:"resolved"`
		} `json:"packages"`
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, errors.New(path + ": " + err.Error())
	}
	var out []lockedPackage
	for key, p := range lock.Packages {
		i := strings.LastIndex(key, "node_modules/")
		if i < 0 || p.Link || p.Version == "" || strings.HasPrefix(p.Resolved, "file:") {
			continue
		}
		out = append(out, lockedPackage{Name: key[i+len("node_modules/"):], Version: p.Version, File: path})
	}
	if len(lock.Packages) == 0 {
		var walk func(map[string]json.RawMessage)
		walk = func(deps map[string]json.RawMessage) {
			for name, raw := range deps {
				var d struct {
					Version      string                     `json:"version"`
					Dependencies map[string]json.RawMessage `json:"dependencies"`
				}
				if json.Unmarshal(raw, &d) == nil {
					if d.Version != "" && !strings.Contains(d.Version, ":") {
						out = append(out, lockedPackage{Name: name, Version: d.Version, File: path})
					}
					walk(d.Dependencies)
				}
			}
		}
		walk(lock.Dependencies)
	}
	return out, nil
}

// lockedPackages reads yarn.lock and package-lock.json in dir, whichever exist, deduplicated.
func lockedPackages(dir string) ([]lockedPackage, error) {
	var all []lockedPackage
	if ps, err := parseYarnLock(filepath.Join(dir, "yarn.lock")); err == nil {
		all = append(all, ps...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if ps, err := parsePackageLock(filepath.Join(dir, "package-lock.json")); err == nil {
		all = append(all, ps...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seen := map[string]bool{}
	var out []lockedPackage
	for _, p := range all {
		if k := p.Name + "@" + p.Version; !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// manifestDeps lists the dependency names of package.json in dir (all dependency kinds).
func manifestDeps(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var pj map[string]json.RawMessage
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil, errors.New(filepath.Join(dir, "package.json") + ": " + err.Error())
	}
	var names []string
	for _, k := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var m map[string]string
		if raw, ok := pj[k]; ok && json.Unmarshal(raw, &m) == nil {
			for n := range m {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// scopeOf is "@scope" of "@scope/name", "" for an unscoped name.
func scopeOf(name string) string {
	if !strings.HasPrefix(name, "@") {
		return ""
	}
	s, _, ok := strings.Cut(name, "/")
	if !ok {
		return ""
	}
	return s
}
