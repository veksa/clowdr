package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

// parseYarnLock reads a yarn.lock: the yarn 2+ (berry) format or the yarn 1 (classic) one. A
// file in neither format is an error, so a check never passes on a lockfile it could not read.
func parseYarnLock(path string) ([]lockedPackage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	switch {
	case strings.Contains(text, "\n__metadata:\n") || strings.HasPrefix(text, "__metadata:\n"):
		return parseBerryLock(path, text), nil
	case strings.Contains(text, "# yarn lockfile v1"):
		return parseClassicLock(path, text), nil
	}
	return nil, fmt.Errorf("%s: unknown yarn.lock format (expected yarn 1 or yarn 2+)", path)
}

// parseBerryLock reads a yarn 2+ lockfile: every top-level entry has a "resolution" like
// "@scope/name@npm:1.2.3", with the real name also for an alias. Entries of other protocols
// (workspace:, patch:, git, file) are not from a registry and are skipped.
func parseBerryLock(path, text string) []lockedPackage {
	var out []lockedPackage
	for _, line := range strings.Split(text, "\n") {
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
	return out
}

// parseClassicLock reads a yarn 1 lockfile. An entry is a line of specifiers ("name@range",
// "alias@npm:@scope/name@range") followed by indented fields; the real name comes from the
// resolved tarball URL ("/@scope/name/-/name-1.0.0.tgz"), else from the specifier.
func parseClassicLock(path, text string) []lockedPackage {
	var out []lockedPackage
	var spec, version, resolved string
	flush := func() {
		if spec != "" && version != "" {
			if name := classicName(spec, resolved); name != "" {
				out = append(out, lockedPackage{Name: name, Version: version, File: path})
			}
		}
		spec, version, resolved = "", "", ""
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case !strings.HasPrefix(line, " "):
			flush()
			spec = strings.TrimSuffix(line, ":")
		case strings.HasPrefix(line, "  version "):
			version = strings.Trim(strings.TrimPrefix(line, "  version "), `"`)
		case strings.HasPrefix(line, "  resolved "):
			resolved = strings.Trim(strings.TrimPrefix(line, "  resolved "), `"`)
		}
	}
	flush()
	return out
}

func classicName(spec, resolved string) string {
	if u, err := url.Parse(resolved); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		p := u.EscapedPath()
		if i := strings.Index(p, "/-/"); i > 0 {
			seg := p[:i]
			if j := strings.LastIndex(seg, "/@"); j >= 0 {
				seg = seg[j+1:]
			} else if j := strings.LastIndex(seg, "%40"); j >= 0 && strings.Contains(strings.ToLower(seg[j:]), "%2f") {
				seg = seg[j:]
			} else {
				seg = seg[strings.LastIndex(seg, "/")+1:]
			}
			if name, err := url.PathUnescape(seg); err == nil && name != "" {
				return name
			}
		}
	}
	first, _, _ := strings.Cut(spec, ", ")
	first = strings.Trim(strings.TrimSpace(first), `"`)
	if i := strings.Index(first, "@npm:"); i > 0 {
		first = first[i+len("@npm:"):]
	}
	return specName(first)
}

// specName is the package name of "name@range" or "@scope/name@range"; "" for non-registry
// specifiers (file:, link:, git URLs).
func specName(spec string) string {
	if strings.ContainsAny(spec, ":") {
		return ""
	}
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return spec
	}
	return spec[:i]
}

// parsePackageLock reads package-lock.json (lockfile v2 and v3 "packages", v1 "dependencies").
func parsePackageLock(path string) ([]lockedPackage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Name     string `json:"name"` // the real name of an aliased package
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
		name := key[i+len("node_modules/"):]
		if p.Name != "" {
			name = p.Name
		}
		out = append(out, lockedPackage{Name: name, Version: p.Version, File: path})
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
					switch v, ok := strings.CutPrefix(d.Version, "npm:"); {
					case ok: // an alias: "npm:@scope/name@1.0.0"
						if i := strings.LastIndex(v, "@"); i > 0 {
							out = append(out, lockedPackage{Name: v[:i], Version: v[i+1:], File: path})
						}
					case d.Version != "" && !strings.Contains(d.Version, ":"):
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

// lockfileNames are the lockfiles lockedPackages reads; unsupportedLockfiles are recognised but
// cannot be read, so a check refuses to pass on them.
var (
	lockfileNames        = []string{"yarn.lock", "package-lock.json", "npm-shrinkwrap.json"}
	unsupportedLockfiles = []string{"pnpm-lock.yaml", "bun.lock", "bun.lockb"}
)

// lockedPackages reads yarn.lock, package-lock.json and npm-shrinkwrap.json in dir, whichever
// exist, deduplicated.
func lockedPackages(dir string) ([]lockedPackage, error) {
	var all []lockedPackage
	if ps, err := parseYarnLock(filepath.Join(dir, "yarn.lock")); err == nil {
		all = append(all, ps...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, f := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if ps, err := parsePackageLock(filepath.Join(dir, f)); err == nil {
			all = append(all, ps...)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
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

// manifestDeps lists the packages package.json in dir depends on, in every dependency kind, by
// their real names: "tool": "npm:@acme/tool@^1" counts as @acme/tool. The package.json files of
// the workspaces it declares are read too.
func manifestDeps(dir string) ([]string, error) {
	seen := map[string]bool{}
	if err := collectManifestDeps(dir, seen, true); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func collectManifestDeps(dir string, seen map[string]bool, workspaces bool) error {
	file := filepath.Join(dir, "package.json")
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var pj map[string]json.RawMessage
	if err := json.Unmarshal(b, &pj); err != nil {
		return errors.New(file + ": " + err.Error())
	}
	for _, k := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var m map[string]string
		if raw, ok := pj[k]; ok && json.Unmarshal(raw, &m) == nil {
			for n, spec := range m {
				seen[n] = true
				if target, ok := strings.CutPrefix(spec, "npm:"); ok {
					if name := specName(target); name != "" {
						seen[name] = true
					}
				}
			}
		}
	}
	if !workspaces {
		return nil
	}
	var patterns []string
	if raw, ok := pj["workspaces"]; ok && json.Unmarshal(raw, &patterns) != nil {
		var ws struct {
			Packages []string `json:"packages"`
		}
		json.Unmarshal(raw, &ws)
		patterns = ws.Packages
	}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(pat, "/"))))
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.IsDir() {
				if err := collectManifestDeps(m, seen, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
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
