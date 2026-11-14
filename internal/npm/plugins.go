package npm

// The Claude Code plugin marketplace at /plugins/ (docs/plugins.md). A plugin is a package of
// this registry whose package.json says "claude-plugin": true; the marketplace lists, for one
// token, the latest version of every plugin package the token may read, and serves each version
// as a zip built from its tarball. The same npm tokens work here, and they need the
// plugins:read scope on top of read rights on the package.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/store"
)

const (
	// pluginManifest is where a plugin's manifest sits, relative to the package root.
	pluginManifest = ".claude-plugin/plugin.json"
	// pluginManifestMax caps plugin.json.
	pluginManifestMax = 1 << 20
	// Claude Code's limits on an archive plugin: entries extracted and bytes downloaded.
	pluginMaxEntries = 100_000
	pluginMaxZip     = 256 << 20
	// pluginMaxRatio is Claude Code's limit on how much an archive may expand. An entry that
	// deflates better than this is stored instead, so no zip built here comes near it.
	pluginMaxRatio = 50
)

// pluginNameRe is a plugin name Claude Code installs (letters, digits, '.', '_', '-', starting
// with a letter or digit, at most 128 characters), narrowed to what a package name allows.
var pluginNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// pluginTar collects, while checkTarball reads a tarball, what the plugin rules need. It does not
// know yet whether the package is a plugin: package.json may come later in the archive.
type pluginTar struct {
	files    int
	special  string // the first entry that is neither a regular file nor a directory
	manifest []byte // .claude-plugin/plugin.json, when it is a regular file
	bad      string // why .claude-plugin/plugin.json cannot be used
	err      error  // a read error (the unpack limit)
}

func (p *pluginTar) entry(rel string, hdr *tar.Header, r io.Reader) error {
	if hdr.Typeflag != tar.TypeReg {
		if p.special == "" {
			p.special = rel
		}
		if rel == pluginManifest {
			p.bad = pluginManifest + " in the tarball is not a regular file"
		}
		return nil
	}
	p.files++
	if rel != pluginManifest {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r, pluginManifestMax+1))
	switch {
	case errors.Is(err, errTooLarge):
		p.err = err
		return err
	case err != nil:
		p.bad = pluginManifest + " in the tarball cannot be read"
	case len(b) > pluginManifestMax:
		p.bad = fmt.Sprintf("%s in the tarball is larger than %d bytes", pluginManifest, pluginManifestMax)
	default:
		p.manifest = b
	}
	return nil
}

// checkPlugin applies the plugin rules to a package whose package.json carries flag as
// "claude-plugin": absent or false is an ordinary package; true makes it a plugin, which needs a
// valid .claude-plugin/plugin.json whose name is the package's unscoped name and whose version
// is the package's version (Claude Code updates a plugin by that version).
func checkPlugin(name, version string, flag json.RawMessage, p *pluginTar) (*store.NPMPlugin, error) {
	switch strings.TrimSpace(string(flag)) {
	case "", "false":
		return nil, nil
	case "true":
	default:
		return nil, errors.New(`"claude-plugin" in package.json must be true or false`)
	}
	_, base, _ := strings.Cut(name, "/")
	if !pluginNameRe.MatchString(base) {
		return nil, fmt.Errorf("plugin %s: the name after the scope may use only a-z, 0-9, '.', '_' and '-', at most 128 characters (Claude Code's plugin names)", name)
	}
	if p.special != "" {
		return nil, fmt.Errorf("plugin %s: %s is a link or a special file; a plugin holds only regular files and directories", name, p.special)
	}
	if p.files > pluginMaxEntries {
		return nil, fmt.Errorf("plugin %s: %d files, Claude Code extracts at most %d", name, p.files, pluginMaxEntries)
	}
	if p.bad != "" {
		return nil, fmt.Errorf("plugin %s: %s", name, p.bad)
	}
	if p.manifest == nil {
		return nil, fmt.Errorf(`plugin %s: "claude-plugin": true needs %s in the package`, name, pluginManifest)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(p.manifest, &m); err != nil || m == nil {
		return nil, fmt.Errorf("plugin %s: %s is not a JSON object", name, pluginManifest)
	}
	str := func(k string) (string, bool, error) {
		raw, ok := m[k]
		if !ok {
			return "", false, nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true, fmt.Errorf("plugin %s: %q in %s is not a string", name, k, pluginManifest)
		}
		return s, true, nil
	}
	pn, ok, err := str("name")
	if err != nil {
		return nil, err
	}
	if !ok || pn == "" {
		return nil, fmt.Errorf("plugin %s: %s has no name", name, pluginManifest)
	}
	if pn != base {
		return nil, fmt.Errorf("plugin %s: %s is named %q, it must be %q (the package name without its scope)", name, pluginManifest, pn, base)
	}
	pv, ok, err := str("version")
	if err != nil {
		return nil, err
	}
	if !ok || pv == "" {
		return nil, fmt.Errorf("plugin %s: %s has no version; set it to %s (Claude Code updates a plugin only when this version changes)", name, pluginManifest, version)
	}
	if pv != version {
		return nil, fmt.Errorf("plugin %s: %s says version %s, the package %s; they must be equal (Claude Code updates by the plugin.json version)", name, pluginManifest, pv, version)
	}
	desc, _, err := str("description")
	if err != nil {
		return nil, err
	}
	return &store.NPMPlugin{Name: base, Description: desc}, nil
}

// zipModified is the modification time of every zip entry: the earliest an MS-DOS date can say,
// so the bytes depend only on the files.
var zipModified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

type zipFile struct {
	name string
	exec bool
	data []byte
}

// buildZip turns an npm tarball into the zip Claude Code installs: the top folder ("package/")
// stripped, so the plugin root is the top of the zip; regular files only, sorted by path; a fixed
// timestamp; mode 0755 for a file with any execute bit, else 0644. The same tarball always gives
// the same bytes (for one Go release: deflate's output is not promised across releases, which is
// why the digest is kept with the version and refreshed whenever the zip is rebuilt).
func buildZip(tgz []byte, unpacked int64) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, errors.New("the tarball is not gzip")
	}
	tr := tar.NewReader(&capReader{r: gz, n: unpacked})
	var files []zipFile
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the tarball: %w", err)
		}
		rel, err := extractedPath(hdr.Name)
		if err != nil {
			return nil, err
		}
		if rel == "" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if seen[rel] {
			return nil, fmt.Errorf("the tarball has %s twice", rel)
		}
		seen[rel] = true
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		files = append(files, zipFile{name: rel, exec: hdr.Mode&0o111 != 0, data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, flate.BestCompression)
	})
	var comp bytes.Buffer
	for _, f := range files {
		// Deflate, unless extraction would expand the entry more than pluginMaxRatio times or
		// deflate does not shrink it: then store it.
		comp.Reset()
		fw, _ := flate.NewWriter(&comp, flate.BestCompression)
		fw.Write(f.data)
		fw.Close()
		fh := &zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: zipModified}
		if n := int64(comp.Len()); len(f.data) == 0 || n*pluginMaxRatio < int64(len(f.data)) || n >= int64(len(f.data)) {
			fh.Method = zip.Store
		}
		mode := os.FileMode(0o644)
		if f.exec {
			mode = 0o755
		}
		fh.SetMode(mode)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (h *Handler) zipPath(sha512hex string) (string, bool) {
	if !hexRe.MatchString(sha512hex) {
		return "", false
	}
	return filepath.Join(h.Dir, "zips", sha512hex+".zip"), true
}

// writeZip stores a plugin zip next to the tarballs, keyed by the tarball's sha512, atomically.
func (h *Handler) writeZip(sha512hex string, data []byte) error {
	path, ok := h.zipPath(sha512hex)
	if !ok {
		return errors.New("bad hash")
	}
	return writeAtomic(path, data)
}

// makeZip builds the zip of a plugin version from its tarball, stores it and returns its digest
// and size.
func (h *Handler) makeZip(sha512hex string, tgz []byte, unpacked int64) (string, int64, error) {
	z, err := buildZip(tgz, unpacked)
	if err != nil {
		return "", 0, err
	}
	if len(z) > pluginMaxZip {
		return "", 0, fmt.Errorf("the plugin zip is %d bytes, Claude Code downloads at most %d", len(z), pluginMaxZip)
	}
	if err := h.writeZip(sha512hex, z); err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(z)
	return hex.EncodeToString(sum[:]), int64(len(z)), nil
}

// manifestSaysPlugin reports whether a stored version object carries "claude-plugin": true: a
// hint, for versions published before clowdrd knew plugins, that the tarball is worth checking.
func manifestSaysPlugin(m json.RawMessage) bool {
	var v struct {
		ClaudePlugin bool `json:"claude-plugin"`
	}
	return len(m) > 0 && json.Unmarshal(m, &v) == nil && v.ClaudePlugin
}

// pluginOf returns a version's plugin record with its zip on disk, building the zip when it is
// missing (deleted by hand, or never built), and checking a version published before this server
// knew plugins. It returns nil for a version that is not a plugin.
func (h *Handler) pluginOf(v store.NPMVersion) (*store.NPMPlugin, error) {
	if v.Removed || (v.Plugin == nil && !manifestSaysPlugin(v.Manifest)) {
		return nil, nil
	}
	zp, okZip := h.zipPath(v.SHA512)
	tp, okTar := h.tarPath(v.SHA512)
	if !okZip || !okTar {
		return nil, errors.New("bad hash")
	}
	if v.Plugin != nil && v.Plugin.ZipSHA256 != "" {
		if _, err := os.Stat(zp); err == nil {
			return v.Plugin, nil
		}
	}
	h.zipMu.Lock()
	defer h.zipMu.Unlock()
	if v.Plugin == nil && h.notPlugin[v.SHA512] {
		return nil, nil
	}
	// Another request may have built it while this one waited.
	if cur, err := h.Store.NPMVersion(v.Name, v.Version); err == nil && cur.Plugin != nil && cur.Plugin.ZipSHA256 != "" {
		if _, err := os.Stat(zp); err == nil {
			return cur.Plugin, nil
		}
	}
	tgz, err := os.ReadFile(tp)
	if err != nil {
		return nil, fmt.Errorf("tarball: %w", err)
	}
	p := v.Plugin
	if p == nil {
		p, err = checkTarball(tgz, v.Name, v.Version, maxUnpackedCap)
		if err != nil || p == nil {
			if h.notPlugin == nil {
				h.notPlugin = map[string]bool{}
			}
			h.notPlugin[v.SHA512] = true
			if err != nil {
				h.log().Warn("npm: not listed as a plugin", "package", v.Name, "version", v.Version, "err", err)
			}
			return nil, nil
		}
	}
	np := *p
	np.ZipSHA256, np.ZipSize, err = h.makeZip(v.SHA512, tgz, maxUnpackedCap)
	if err != nil {
		return nil, err
	}
	err = h.Store.UpdateNPM(v.Name, func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		for _, o := range vs {
			if o.Version == v.Version && !o.Removed {
				o.Plugin = &np
				return []store.NPMVersion{o}, nil
			}
		}
		return nil, store.ErrNotFound
	})
	if err != nil {
		return nil, err
	}
	h.log().Info("npm: plugin zip built", "package", v.Name, "version", v.Version, "sha256", np.ZipSHA256)
	return &np, nil
}

// Marketplace is the marketplace.json Claude Code reads
// (https://code.claude.com/docs/en/plugins/marketplace-reference).
type Marketplace struct {
	Name        string              `json:"name"`
	Owner       MarketplaceOwner    `json:"owner"`
	Description string              `json:"description,omitempty"`
	Plugins     []MarketplacePlugin `json:"plugins"`
}

type MarketplaceOwner struct {
	Name string `json:"name"`
}

type MarketplacePlugin struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Version     string        `json:"version"`
	Source      ArchiveSource `json:"source"`
}

// ArchiveSource is a plugin source of type "archive": a zip over https with its digest.
type ArchiveSource struct {
	Source string `json:"source"` // "archive"
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// MarketplaceName is the marketplace's name: plugins.name from platform.yml, else "clowdr-" and
// the server's domain with every run of other characters than a-z and 0-9 made one dash
// ("example.com" → "clowdr-example-com"). Without a domain, the host the client used stands in.
func MarketplaceName(c config.PluginsConf, domain, host string) string {
	if c.Name != "" {
		return c.Name
	}
	if domain == "" {
		domain = host
		if h, _, err := net.SplitHostPort(host); err == nil {
			domain = h
		}
	}
	s := strings.Trim(nonName.ReplaceAllString(strings.ToLower(domain), "-"), "-")
	if s == "" {
		s = "local"
	}
	return "clowdr-" + s
}

func (h *Handler) pluginsConfig() config.PluginsConf {
	if h.Plugins != nil {
		return h.Plugins()
	}
	return config.PluginsConf{}
}

func (h *Handler) domain() string {
	if h.Domain != nil {
		return h.Domain()
	}
	return ""
}

// servePlugins serves the routes under /plugins/; the token is checked already.
func (c *call) servePlugins(segs []string) {
	r := c.r
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		notFound(c.w)
		return
	}
	switch {
	case len(segs) == 1 && segs[0] == "marketplace.json":
		c.marketplace()
	case len(segs) == 3 && strings.HasPrefix(segs[0], "@"):
		c.pluginZip(segs[0]+"/"+segs[1], segs[2])
	default:
		notFound(c.w)
	}
}

// marketplace serves marketplace.json: one entry per plugin package the token may read, at the
// version its latest tag points at. A token that may read none gets a valid, empty marketplace;
// a token without plugins:read gets 404.
func (c *call) marketplace() {
	if !c.rights.Plugins() {
		c.audit("plugin.list", "", "", "", 404, "no plugins:read")
		notFound(c.w)
		return
	}
	base := BaseURL(c.r)
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	domain := c.h.domain()
	conf := c.h.pluginsConfig()
	owner := conf.Owner
	if owner == "" {
		owner = domain
	}
	if owner == "" {
		owner = host
	}
	m := Marketplace{
		Name:        MarketplaceName(conf, domain, host),
		Owner:       MarketplaceOwner{Name: owner},
		Description: "Private Claude Code plugins on " + host,
		Plugins:     []MarketplacePlugin{},
	}
	pkgs, err := c.h.Store.NPMPackages()
	if err != nil {
		writeErr(c.w, 500, "storage error")
		return
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	byName := map[string]string{}
	var skipped []string
	for _, p := range pkgs {
		if !c.rights.CanRead(p.Name) {
			continue
		}
		pp, live, err := c.h.live(p.Name)
		if err != nil || len(live) == 0 {
			continue
		}
		latest := latestOf(liveTags(pp.DistTags, live), live)
		var v store.NPMVersion
		for _, lv := range live {
			if lv.Version == latest {
				v = lv
			}
		}
		pl, err := c.h.pluginOf(v)
		if err != nil {
			c.h.log().Error("npm: plugin zip", "package", p.Name, "version", v.Version, "err", err)
			continue
		}
		if pl == nil {
			continue
		}
		// Two scopes may hold plugins of the same name; a marketplace lists a name once. The
		// first package by name keeps it, so the answer is stable.
		if first, ok := byName[pl.Name]; ok {
			skipped = append(skipped, p.Name+" (name taken by "+first+")")
			continue
		}
		byName[pl.Name] = p.Name
		scope, name, _ := strings.Cut(p.Name, "/")
		m.Plugins = append(m.Plugins, MarketplacePlugin{
			Name: pl.Name, Description: pl.Description, Version: v.Version,
			Source: ArchiveSource{Source: "archive", URL: base + "/plugins/" + scope + "/" + name + "/" + v.Version + ".zip", SHA256: pl.ZipSHA256},
		})
	}
	sort.Slice(m.Plugins, func(i, j int) bool { return m.Plugins[i].Name < m.Plugins[j].Name })
	detail := fmt.Sprintf("%d plugins", len(m.Plugins))
	if len(skipped) > 0 {
		detail += ", skipped " + strings.Join(skipped, ", ")
	}
	c.audit("plugin.list", "", "", "", 200, detail)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(200)
	if c.r.Method == http.MethodHead {
		return
	}
	enc := json.NewEncoder(c.w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(m)
}

// pluginZip serves /plugins/@scope/name/<version>.zip. Without plugins:read, without read rights
// on the package, for a version that does not exist or is not a plugin, the answer is the same
// 404.
func (c *call) pluginZip(name, file string) {
	ver, ok := strings.CutSuffix(file, ".zip")
	if !c.rights.Plugins() || !c.rights.CanRead(name) || !ok || !validVersion(ver) {
		c.audit("plugin.download", name, "", "", 404, "")
		notFound(c.w)
		return
	}
	v, err := c.h.Store.NPMVersion(name, ver)
	if err != nil || v.Removed {
		c.audit("plugin.download", name, ver, "", 404, "")
		notFound(c.w)
		return
	}
	pl, err := c.h.pluginOf(v)
	if err != nil {
		c.h.log().Error("npm: plugin zip", "package", name, "version", ver, "err", err)
		c.audit("plugin.download", name, ver, "", 404, "zip unavailable")
		notFound(c.w)
		return
	}
	if pl == nil {
		c.audit("plugin.download", name, ver, "", 404, "not a plugin")
		notFound(c.w)
		return
	}
	path, _ := c.h.zipPath(v.SHA512)
	f, err := os.Open(path)
	if err != nil {
		c.h.log().Error("npm: plugin zip missing", "package", name, "version", ver, "err", err)
		c.audit("plugin.download", name, ver, "", 404, "file missing")
		notFound(c.w)
		return
	}
	defer f.Close()
	c.audit("plugin.download", name, ver, "", 200, "")
	c.w.Header().Set("Content-Type", "application/zip")
	// Private, and revalidated: the digest in marketplace.json is the ETag, and a rebuilt zip (a
	// new Go release may deflate differently) gets a new one.
	c.w.Header().Set("Cache-Control", "private, no-cache")
	c.w.Header().Set("ETag", `"`+pl.ZipSHA256+`"`)
	http.ServeContent(c.w, c.r, "", v.Published, f)
}

// writeAtomic writes data to path through a synced temp file in the same directory.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var rnd [6]byte
	rand.Read(rnd[:])
	tmp := filepath.Join(dir, tmpPrefix+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
