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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/store"
)

const (
	// pluginManifest is where a plugin's manifest sits, relative to the package root.
	pluginManifest = ".claude-plugin/plugin.json"
	// pluginManifestMax caps plugin.json.
	pluginManifestMax = 1 << 20
	// pluginMaxEntries is Claude Code's limit on the entries it extracts from an archive.
	pluginMaxEntries = 100_000
	// pluginMaxRatio is Claude Code's limit on how much an archive may expand. An entry that
	// deflates better than this is stored instead, so no zip built here comes near it.
	pluginMaxRatio = 50
	// zipRetry is how long a zip whose tarball cannot make one (over the hard cap, a broken
	// archive) is not tried again; zipRetrySoon the same after a failure of the disk or the
	// store, which may pass by itself.
	zipRetry     = time.Hour
	zipRetrySoon = time.Minute
)

// pluginNameRe is a plugin name Claude Code installs (letters, digits, '.', '_', '-', starting
// with a letter or digit, at most 128 characters), narrowed to what a package name allows.
var pluginNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// errZipUnavailable is a plugin whose zip cannot be built; the reason is logged once.
var errZipUnavailable = errors.New("plugin zip unavailable")

// pluginTar collects, while checkTarball reads a tarball, what the plugin rules need. It does not
// know yet whether the package is a plugin: package.json may come later in the archive.
type pluginTar struct {
	files    int
	special  string // the first entry that is neither a regular file nor a directory
	manifest []byte // .claude-plugin/plugin.json, when it is a regular file
	bad      string // why .claude-plugin/plugin.json cannot be used
	badName  string // the first entry name a plugin may not have, and why
	err      error  // a read error (the unpack limit)

	lowerFiles map[string]string // lower-cased path → path, of files
	lowerDirs  map[string]bool   // lower-cased folders, explicit or implied
}

var driveRe = regexp.MustCompile(`^[A-Za-z]:`)

// windowsReserved are the device names Windows will not create a file under, whatever the
// extension.
var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

func init() {
	for i := 0; i <= 9; i++ {
		windowsReserved["COM"+strconv.Itoa(i)] = true
		windowsReserved["LPT"+strconv.Itoa(i)] = true
	}
	for _, d := range []string{"¹", "²", "³"} {
		windowsReserved["COM"+d] = true
		windowsReserved["LPT"+d] = true
	}
}

// badPart says why a path element cannot be a file name on every system Claude Code runs on.
func badPart(p string) string {
	for _, r := range p {
		switch {
		case r < 0x20 || r == 0x7f:
			return "a control character"
		case r == ':':
			return "a colon"
		case r == '\\':
			return "a backslash"
		case r == utf8.RuneError:
			return "a byte that is not UTF-8"
		}
	}
	if strings.HasSuffix(p, ".") || strings.HasSuffix(p, " ") {
		return "a name ending in a dot or a space"
	}
	base, _, _ := strings.Cut(p, ".")
	if windowsReserved[strings.ToUpper(strings.TrimRight(base, " "))] {
		return "a name Windows reserves"
	}
	return ""
}

// name checks an entry's name for a plugin: no absolute path or backslash in the raw name, no
// element that some system cannot create, and no two entries that are one on a file system that
// ignores case.
func (p *pluginTar) name(rel string, hdr *tar.Header) {
	if p.badName != "" {
		return
	}
	raw := hdr.Name
	switch {
	case strings.HasPrefix(raw, "/") || driveRe.MatchString(raw):
		p.badName = fmt.Sprintf("%q is an absolute path", raw)
		return
	case strings.Contains(raw, `\`):
		p.badName = fmt.Sprintf("%q has a backslash", raw)
		return
	}
	for _, part := range strings.Split(rel, "/") {
		if why := badPart(part); why != "" {
			p.badName = fmt.Sprintf("%q has %s", rel, why)
			return
		}
	}
	if p.lowerFiles == nil {
		p.lowerFiles, p.lowerDirs = map[string]string{}, map[string]bool{}
	}
	l := strings.ToLower(rel)
	clash := func() { p.badName = fmt.Sprintf("%q and another entry differ only in case", rel) }
	for i := strings.IndexByte(l, '/'); i >= 0; i = next(l, i) {
		if _, ok := p.lowerFiles[l[:i]]; ok {
			clash()
			return
		}
		p.lowerDirs[l[:i]] = true
	}
	if hdr.Typeflag == tar.TypeDir {
		if _, ok := p.lowerFiles[l]; ok {
			clash()
			return
		}
		p.lowerDirs[l] = true
		return
	}
	if o, ok := p.lowerFiles[l]; (ok && o != rel) || p.lowerDirs[l] {
		clash()
		return
	}
	p.lowerFiles[l] = rel
}

// next is the index of the next '/' in s after i, or -1.
func next(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
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
	if p.badName != "" {
		return nil, fmt.Errorf("plugin %s: the entry %s; a plugin must unpack the same on every system", name, p.badName)
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

// zipFile is one file of a zip being built, unpacked to a temp file.
type zipFile struct {
	name string
	exec bool
	path string
	size int64
}

// errZipTooLarge is returned by limitWriter past its limit.
var errZipTooLarge = errors.New("zip too large")

// zipContentError is a zip build that failed on what the tarball holds, not on the disk: trying
// again gives the same answer.
type zipContentError struct{ error }

func (e zipContentError) Unwrap() error { return e.error }

func contentErr(format string, a ...any) error { return zipContentError{fmt.Errorf(format, a...)} }

// limitWriter fails once more than n bytes were written.
type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errZipTooLarge
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// built is a zip written to a temp file, not yet in its place.
type built struct {
	tmp    string
	sha256 string
	size   int64
}

// buildZip writes the zip Claude Code installs for an npm tarball to a temp file in dir: the top
// folder ("package/") stripped, so the plugin root is the top of the zip; regular files only,
// sorted by path; a fixed timestamp; mode 0755 for a file with any execute bit, else 0644. Each
// file is deflated once, and stored instead when that would let it expand more than
// pluginMaxRatio times or does not shrink it. Memory stays small whatever the files: each one
// goes through a temp file. The same tarball gives the same bytes (for one Go release: deflate's
// output is not promised across releases, which is why the digest is kept with the version and
// refreshed whenever the zip is rebuilt). A zip larger than maxZip fails with a clear error.
func buildZip(tgz io.Reader, dir string, unpacked, maxZip int64) (b built, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return b, err
	}
	work, err := os.MkdirTemp(dir, tmpPrefix+"build-")
	if err != nil {
		return b, err
	}
	defer os.RemoveAll(work)

	gz, err := gzip.NewReader(tgz)
	if err != nil {
		return b, contentErr("the tarball is not gzip")
	}
	tr := tar.NewReader(&capReader{r: gz, n: unpacked})
	var files []zipFile
	seen := map[string]bool{}
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return b, contentErr("read the tarball: %w", err)
		}
		if err := checkEntryHeader(hdr); err != nil {
			return b, zipContentError{err}
		}
		if total += hdr.Size; total > unpacked {
			return b, contentErr("the tarball unpacks to more than %d bytes", unpacked)
		}
		rel, err := extractedPath(hdr.Name)
		if err != nil {
			return b, zipContentError{err}
		}
		if rel == "" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if seen[rel] {
			return b, contentErr("the tarball has %s twice", rel)
		}
		seen[rel] = true
		f := zipFile{name: rel, exec: hdr.Mode&0o111 != 0, path: filepath.Join(work, strconv.Itoa(len(files)))}
		if f.size, err = copyToFile(f.path, tr); err != nil {
			if errors.Is(err, errTooLarge) {
				return b, contentErr("the tarball unpacks to more than %d bytes", unpacked)
			}
			return b, fmt.Errorf("unpack %s: %w", rel, err)
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	out, err := os.CreateTemp(dir, tmpPrefix+"zip-")
	if err != nil {
		return b, err
	}
	b.tmp = out.Name()
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(b.tmp)
			b = built{}
		}
	}()
	sum := sha256.New()
	zw := zip.NewWriter(&limitWriter{w: io.MultiWriter(out, sum), n: maxZip})
	comp := filepath.Join(work, "deflated")
	for _, f := range files {
		if err := addZipEntry(zw, f, comp); err != nil {
			if errors.Is(err, errZipTooLarge) {
				return b, contentErr("the plugin zip is larger than %d bytes (plugins.max_zip)", maxZip)
			}
			return b, err
		}
	}
	if err := zw.Close(); err != nil {
		if errors.Is(err, errZipTooLarge) {
			return b, contentErr("the plugin zip is larger than %d bytes (plugins.max_zip)", maxZip)
		}
		return b, err
	}
	if err := out.Sync(); err != nil {
		return b, err
	}
	fi, err := out.Stat()
	if err != nil {
		return b, err
	}
	if err := out.Close(); err != nil {
		return b, err
	}
	b.sha256, b.size = hex.EncodeToString(sum.Sum(nil)), fi.Size()
	return b, nil
}

func copyToFile(path string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// addZipEntry deflates f once into the temp file comp, then writes the deflated or the raw bytes.
func addZipEntry(zw *zip.Writer, f zipFile, comp string) error {
	src, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer src.Close()
	cf, err := os.Create(comp)
	if err != nil {
		return err
	}
	defer cf.Close()
	crc := crc32.NewIEEE()
	fw, _ := flate.NewWriter(cf, flate.BestCompression)
	if _, err := io.Copy(io.MultiWriter(fw, crc), src); err != nil {
		return err
	}
	if err := fw.Close(); err != nil {
		return err
	}
	csize, err := cf.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	fh := zipHeader(f, crc)
	body := io.Reader(cf)
	if f.size == 0 || csize*pluginMaxRatio < f.size || csize >= f.size {
		fh.Method, fh.CompressedSize64 = zip.Store, uint64(f.size)
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body = src
	} else {
		fh.Method, fh.CompressedSize64 = zip.Deflate, uint64(csize)
		if _, err := cf.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	w, err := zw.CreateRaw(fh)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, body)
	return err
}

// zipHeader is the header of one entry, as zip.Writer.CreateHeader would make it with Modified
// set to zipModified: CreateRaw leaves the timestamp, the UTF-8 flag and the versions to the
// caller.
func zipHeader(f zipFile, crc hash.Hash32) *zip.FileHeader {
	fh := &zip.FileHeader{Name: f.name, CRC32: crc.Sum32(), UncompressedSize64: uint64(f.size)}
	for _, r := range f.name {
		if r >= 0x80 {
			fh.Flags |= 0x800
			break
		}
	}
	// 1980-01-01 00:00 in MS-DOS form, and the same as an extended timestamp.
	fh.ModifiedDate, fh.ModifiedTime = 1<<5|1, 0 //nolint:staticcheck // CreateRaw does not read Modified
	ext := make([]byte, 9)
	binary.LittleEndian.PutUint16(ext[0:], 0x5455)
	binary.LittleEndian.PutUint16(ext[2:], 5)
	ext[4] = 1
	binary.LittleEndian.PutUint32(ext[5:], uint32(zipModified.Unix()))
	fh.Extra = ext
	mode := os.FileMode(0o644)
	if f.exec {
		mode = 0o755
	}
	fh.SetMode(mode)
	fh.CreatorVersion = fh.CreatorVersion&0xff00 | 20
	fh.ReaderVersion = 20
	return fh
}

func (h *Handler) zipPath(sha512hex string) (string, bool) {
	if !hexRe.MatchString(sha512hex) {
		return "", false
	}
	return filepath.Join(h.Dir, "zips", sha512hex+".zip"), true
}

func (h *Handler) maxZip() int64 {
	m := int64(h.pluginsConfig().MaxZip)
	if m <= 0 {
		m = config.DefaultPluginMaxZip
	}
	return min(m, config.PluginMaxZipCap)
}

// makeZip builds the zip of a plugin tarball into a temp file next to the stored zips; place
// moves it into its place. A publish passes plugins.max_zip; a rebuild of a version already
// accepted passes the hard cap, so lowering the setting never delists what is published.
func (h *Handler) makeZip(tgz []byte, unpacked, maxZip int64) (built, error) {
	return buildZip(bytes.NewReader(tgz), filepath.Join(h.Dir, "zips"), unpacked, maxZip)
}

// place renames a built zip to the zip of the tarball sha512hex.
func (h *Handler) place(b built, sha512hex string) error {
	path, ok := h.zipPath(sha512hex)
	if !ok {
		os.Remove(b.tmp)
		return errors.New("bad hash")
	}
	if err := os.Rename(b.tmp, path); err != nil {
		os.Remove(b.tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// manifestSaysPlugin reports whether a stored version object carries "claude-plugin": true. Only a
// hint, for versions stored before the server checked plugins, that the tarball is worth reading:
// from 0.5 on the stored value is the tarball's, and the client's never counts.
func manifestSaysPlugin(m json.RawMessage) bool {
	var v struct {
		ClaudePlugin bool `json:"claude-plugin"`
	}
	return len(m) > 0 && json.Unmarshal(m, &v) == nil && v.ClaudePlugin
}

// mayBePlugin reports whether a version could be a plugin: checked and found one, or never
// checked and its version object hints at it. A version checked and found not to be one is not,
// whatever its version object says.
func mayBePlugin(v store.NPMVersion) bool {
	switch {
	case v.Removed:
		return false
	case v.Plugin != nil:
		return true
	case v.PluginChecked:
		return false
	}
	return manifestSaysPlugin(v.Manifest)
}

// pluginOf returns a version's plugin record with its zip on disk, building the zip when it is
// missing (deleted by hand, or never built), and checking, once, a version stored before the
// server checked plugins. It returns nil for a version that is not a plugin (or no longer exists),
// and errZipUnavailable for one whose zip cannot be built now: logged once, and not tried again
// for zipRetry (the tarball cannot make one) or zipRetrySoon (the disk or the store failed).
//
// The stored digest never describes other bytes than the file: a zip is built under a temp name,
// the record takes its digest, and only then is the file renamed into place. A reader that sees
// the record before the file goes the slow way, waits for the lock, and finds both.
func (h *Handler) pluginOf(v store.NPMVersion) (*store.NPMPlugin, error) {
	if !mayBePlugin(v) {
		return nil, nil
	}
	zp, okZip := h.zipPath(v.SHA512)
	tp, okTar := h.tarPath(v.SHA512)
	if !okZip || !okTar {
		return nil, errZipUnavailable
	}
	if v.Plugin != nil && v.Plugin.ZipSHA256 != "" {
		if _, err := os.Stat(zp); err == nil {
			return v.Plugin, nil
		}
	}
	h.zipMu.Lock()
	defer h.zipMu.Unlock()
	now := h.now()
	if until, ok := h.notPlugin[v.SHA512]; ok && now.Before(until) {
		return nil, nil
	}
	if until, ok := h.zipFailed[v.SHA512]; ok && now.Before(until) {
		return nil, errZipUnavailable
	}
	// Another request may have built it, or retention removed the version, while this one waited.
	cur, err := h.Store.NPMVersion(v.Name, v.Version)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !mayBePlugin(cur)) {
		return nil, nil
	}
	fail := func(what string, err error) (*store.NPMPlugin, error) {
		retry := zipRetrySoon
		var ce zipContentError
		if errors.As(err, &ce) {
			retry = zipRetry
		}
		if h.zipFailed == nil {
			h.zipFailed = map[string]time.Time{}
		}
		h.zipFailed[v.SHA512] = now.Add(retry)
		h.log().Error("npm: plugin zip", "package", v.Name, "version", v.Version, "step", what, "err", err, "retry_after", retry)
		return nil, errZipUnavailable
	}
	if err != nil {
		return fail("read record", err)
	}
	if cur.Plugin != nil && cur.Plugin.ZipSHA256 != "" {
		if _, err := os.Stat(zp); err == nil {
			return cur.Plugin, nil
		}
	}
	tgz, err := os.ReadFile(tp)
	if errors.Is(err, os.ErrNotExist) {
		// Removed between the record and the file: a retention in progress, not a failure.
		if cur, err := h.Store.NPMVersion(v.Name, v.Version); err != nil || cur.Removed {
			return nil, nil
		}
	}
	if err != nil {
		return fail("read tarball", err)
	}
	// update writes the version's plugin fields; false means the version is gone.
	update := func(p *store.NPMPlugin) (bool, error) {
		err := h.Store.UpdateNPM(v.Name, func(_ *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
			for _, o := range vs {
				if o.Version == v.Version && !o.Removed {
					o.Plugin, o.PluginChecked = p, true
					return []store.NPMVersion{o}, nil
				}
			}
			return nil, store.ErrNotFound
		})
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	p := cur.Plugin
	if p == nil {
		// Stored before the server checked plugins: the tarball decides, once.
		p, err = checkTarball(tgz, v.Name, v.Version, maxUnpackedCap)
		if err != nil || p == nil {
			if err != nil {
				h.log().Warn("npm: not a plugin", "package", v.Name, "version", v.Version, "err", err)
			}
			if _, uerr := update(nil); uerr != nil {
				// Not recorded: remember it for a while instead.
				if h.notPlugin == nil {
					h.notPlugin = map[string]time.Time{}
				}
				h.notPlugin[v.SHA512] = now.Add(zipRetrySoon)
			}
			return nil, nil
		}
	}
	b, err := h.makeZip(tgz, maxUnpackedCap, config.PluginMaxZipCap)
	if err != nil {
		return fail("build", err)
	}
	np := *p
	np.ZipSHA256, np.ZipSize = b.sha256, b.size
	ok, err := update(&np)
	if !ok {
		os.Remove(b.tmp)
		if err == nil {
			return nil, nil // removed meanwhile
		}
		return fail("record", err)
	}
	if err := h.place(b, v.SHA512); err != nil {
		return fail("place", err)
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

// MarketplaceName is the marketplace's name (config.MarketplaceName).
func MarketplaceName(c config.PluginsConf, domain, host string) string {
	return config.MarketplaceName(c, domain, host)
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

// pluginsBase is the scheme and host of the marketplace's URLs. With a domain it is the deploy
// host: nginx sends any unknown Host to the deploy host, so the request's Host is not to be
// trusted for links. Its port is kept when the request named the deploy host itself, so a
// server reached on another port than 443 (behind a port mapping) gets links that work there.
// Without a domain, it is what the client used.
func (c *call) pluginsBase() (base, host string) {
	reqBase := BaseURL(c.r)
	reqHost := strings.TrimPrefix(strings.TrimPrefix(reqBase, "https://"), "http://")
	d := c.h.domain()
	if d == "" {
		return reqBase, reqHost
	}
	host = "deploy." + d
	if h, port, err := net.SplitHostPort(reqHost); err == nil && strings.EqualFold(h, host) && port != "443" && port != "" {
		host = net.JoinHostPort(host, port)
	}
	return "https://" + host, host
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
	case len(segs) == 2 && segs[0] == "-" && segs[1] == "clashes":
		c.clashes()
	case len(segs) == 3 && strings.HasPrefix(segs[0], "@"):
		c.pluginZip(segs[0]+"/"+segs[1], segs[2])
	default:
		notFound(c.w)
	}
}

// Clash is a plugin name a token's marketplace leaves out because more than one package the
// token reads has been a plugin of that name.
type Clash struct {
	Name     string   `json:"name"`
	Packages []string `json:"packages"`
}

// Clashes is the answer of /plugins/-/clashes.
type Clashes struct {
	Clashes []Clash `json:"clashes"`
}

// listing works out what a token's marketplace lists. Each plugin name goes by its claimants, the
// packages that have ever been a plugin of it as the server checked them (store claims, never
// removed), among those the token reads:
//
//   - one claimant whose latest version is a plugin with its zip: listed;
//   - more than one claimant, whatever their latest versions are now: nothing under that name;
//   - one claimant whose latest version is not a plugin, or whose zip is unavailable: nothing.
//
// So a plugin name never passes from one package to another for any token: Claude Code keys an
// installed plugin by its name, and a name that changed hands would auto-update every machine
// that installed it to the other package's code. A name can only disappear. Which packages count
// is decided by what the token reads, so nothing about a package leaks to a token that cannot
// read it, and no publish anywhere is judged by other scopes.
func (c *call) listing(urlBase string) (plugins []MarketplacePlugin, clashes []Clash, skipped []string, err error) {
	pkgs, err := c.h.Store.NPMPackages()
	if err != nil {
		return nil, nil, nil, err
	}
	claims, err := c.h.Store.NPMPluginClaims()
	if err != nil {
		return nil, nil, nil, err
	}
	readable := map[string]bool{}
	for _, p := range pkgs {
		if c.rights.CanRead(p.Name) {
			readable[p.Name] = true
		}
	}
	claimants := map[string][]string{} // plugin name → readable packages that claim it
	claimed := map[string]bool{}
	for _, cl := range claims {
		if readable[cl.Package] && !claimed[cl.Package] {
			claimed[cl.Package] = true
			claimants[cl.Plugin] = append(claimants[cl.Plugin], cl.Package)
		}
	}
	// The latest version of each readable claimant, and of each readable package not claimed
	// yet (a 0.4 version is checked, and claims its name, the first time it is looked at).
	type state struct {
		entry *MarketplacePlugin
		why   string
	}
	states := map[string]state{}
	for _, p := range pkgs {
		if !readable[p.Name] {
			continue
		}
		pp, live, err := c.h.live(p.Name)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(live) == 0 {
			states[p.Name] = state{why: "no live version"}
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
		switch {
		case err != nil:
			states[p.Name] = state{why: p.Name + "@" + v.Version + " (zip unavailable)"}
		case pl == nil:
			states[p.Name] = state{why: p.Name + "@" + v.Version + " (not a plugin)"}
		default:
			scope, name, _ := strings.Cut(p.Name, "/")
			states[p.Name] = state{entry: &MarketplacePlugin{
				Name: pl.Name, Description: pl.Description, Version: v.Version,
				Source: ArchiveSource{Source: "archive", URL: urlBase + "/plugins/" + scope + "/" + name + "/" + v.Version + ".zip", SHA256: pl.ZipSHA256},
			}}
			if !claimed[p.Name] {
				// Claimed by the check pluginOf just did (or by a write that will be backfilled).
				claimed[p.Name] = true
				claimants[pl.Name] = append(claimants[pl.Name], p.Name)
			}
		}
	}
	for name, cs := range claimants {
		sort.Strings(cs)
		switch st := states[cs[0]]; {
		case len(cs) > 1:
			clashes = append(clashes, Clash{Name: name, Packages: cs})
			skipped = append(skipped, "plugin name "+name+" in "+strings.Join(cs, " and "))
			c.h.clash(c.tok.Name, name, cs)
		case st.entry != nil && st.entry.Name == name:
			plugins = append(plugins, *st.entry)
		case st.why != "":
			skipped = append(skipped, st.why)
		}
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	sort.Slice(clashes, func(i, j int) bool { return clashes[i].Name < clashes[j].Name })
	sort.Strings(skipped)
	return plugins, clashes, skipped, nil
}

// marketplace serves marketplace.json: one entry per plugin name the token may be given (see
// listing), at the version the package's latest tag points at. A token that may read none gets a
// valid, empty marketplace; a token without plugins:read gets 404.
func (c *call) marketplace() {
	if !c.rights.Plugins() {
		c.audit("plugin.list", "", "", "", 404, "no plugins:read")
		notFound(c.w)
		return
	}
	urlBase, host := c.pluginsBase()
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
	plugins, _, skipped, err := c.listing(urlBase)
	if err != nil {
		writeErr(c.w, 500, "storage error")
		return
	}
	m.Plugins = append(m.Plugins, plugins...)
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

// clashes serves /plugins/-/clashes: the plugin names this token's marketplace leaves out because
// more than one package the token reads has been a plugin of the name, with those packages.
// clowctl shows them; Claude Code does not read it. The same rights as the marketplace apply.
func (c *call) clashes() {
	if !c.rights.Plugins() {
		notFound(c.w)
		return
	}
	urlBase, _ := c.pluginsBase()
	_, clashes, _, err := c.listing(urlBase)
	if err != nil {
		writeErr(c.w, 500, "storage error")
		return
	}
	if clashes == nil {
		clashes = []Clash{}
	}
	writeJSON(c.w, 200, Clashes{Clashes: clashes})
}

// clash logs, once per process, token and set of packages, a plugin name that two packages the
// token reads have claimed.
func (h *Handler) clash(token, name string, pkgs []string) {
	key := token + "\x00" + name + "\x00" + strings.Join(pkgs, " ")
	h.zipMu.Lock()
	defer h.zipMu.Unlock()
	if h.collided[key] {
		return
	}
	if h.collided == nil {
		h.collided = map[string]bool{}
	}
	h.collided[key] = true
	h.log().Warn("npm: a token reads two packages that have been plugins of one name; neither is listed for it (narrow the token's scopes)",
		"token", token, "plugin", name, "packages", strings.Join(pkgs, " "))
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
