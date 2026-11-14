package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/store"
)

// publishBody is what `npm publish` and `yarn npm publish` send. Other top-level fields
// (description, readme, access, ...) are ignored: the document is rebuilt from the versions.
type publishBody struct {
	ID          string                     `json:"_id"`
	Name        string                     `json:"name"`
	DistTags    map[string]string          `json:"dist-tags"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Attachments map[string]attachment      `json:"_attachments"`
}

type attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int64  `json:"length"`
}

type versionManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Integrity string `json:"integrity"`
		Shasum    string `json:"shasum"`
	} `json:"dist"`
}

// A tarball may unpack to 4 times npm.max_publish, at most 512 MiB, while it is checked: a
// small gzip bomb must not keep the server busy inflating it.
const (
	unpackFactor   = 4
	maxUnpackedCap = 512 << 20
	// publishSlots is how many publishes may read and check their bodies at once.
	publishSlots = 2
)

func unpackLimit(maxPublish int64) int64 {
	return min(unpackFactor*maxPublish, maxUnpackedCap)
}

func (h *Handler) publishSlots() chan struct{} {
	h.slotsOnce.Do(func() { h.slots = make(chan struct{}, publishSlots) })
	return h.slots
}

var errTooLarge = errors.New("too large")

// capReader returns errTooLarge once more than n bytes were read.
type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.n+1 {
		p = p[:c.n+1]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	if c.n < 0 {
		return k, errTooLarge
	}
	return k, err
}

var tagRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

// validTag refuses tags npm would read as a version or range (they start with a digit or "v1").
func validTag(t string) bool { return tagRe.MatchString(t) }

func (c *call) publish(name string) {
	w := c.w
	if !c.rights.CanRead(name) {
		c.audit("publish", name, "", "", 404, "")
		notFound(w)
		return
	}
	cfg := c.h.config()
	if !cfg.Publish {
		c.audit("publish", name, "", "", 405, "publishing is off")
		writeErr(w, http.StatusMethodNotAllowed, "publishing is off on this server (npm.publish in platform.yml)")
		return
	}
	// The token can read the package, so a 403 tells it nothing it could not learn already;
	// without read rights the same request got the 404 above.
	if !c.rights.CanPublish(name) {
		c.audit("publish", name, "", "", 403, "no publish right")
		writeErr(w, http.StatusForbidden, "this token cannot publish "+name)
		return
	}
	max := int64(cfg.MaxPublish)
	if c.r.ContentLength > max {
		c.audit("publish", name, "", "", 413, "")
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the publish is larger than %d bytes (npm.max_publish)", max))
		return
	}
	slots := c.h.publishSlots()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-c.r.Context().Done():
		return
	}
	var body publishBody
	if err := json.NewDecoder(http.MaxBytesReader(w, c.r.Body, max)).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.audit("publish", name, "", "", 413, "")
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the publish is larger than %d bytes (npm.max_publish)", max))
			return
		}
		c.audit("publish", name, "", "", 400, "bad json")
		writeErr(w, http.StatusBadRequest, "the body is not a publish document")
		return
	}
	v, tags, data, err := checkPublish(name, body, unpackLimit(max))
	if err != nil {
		c.audit("publish", name, v.Version, "", 400, err.Error())
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := c.h.now()
	v.Published, v.PublishedBy = now, c.tok.Name

	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if old, err := c.h.Store.NPMVersion(name, v.Version); err == nil {
		msg := name + "@" + v.Version + " is already published; versions are immutable, publish a new one"
		if old.Removed {
			msg = name + "@" + v.Version + " was published before and removed by retention; publish a new version"
		}
		c.audit("publish", name, v.Version, "", 409, "exists")
		writeErr(w, http.StatusConflict, msg)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, 500, "storage error")
		return
	}
	if err := c.h.writeTarball(v.SHA512, data); err != nil {
		c.h.log().Error("npm: write tarball", "package", name, "version", v.Version, "err", err)
		writeErr(w, 500, "storage error")
		return
	}
	if v.Plugin != nil {
		var err error
		v.Plugin.ZipSHA256, v.Plugin.ZipSize, err = c.h.makeZip(v.SHA512, data, unpackLimit(max))
		if err != nil {
			c.h.log().Error("npm: plugin zip", "package", name, "version", v.Version, "err", err)
			c.audit("publish", name, v.Version, "", 400, err.Error())
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := c.h.Store.PublishNPM(v, tags); err != nil {
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusConflict, name+"@"+v.Version+" is already published")
			return
		}
		c.h.log().Error("npm: publish", "package", name, "version", v.Version, "err", err)
		writeErr(w, 500, "storage error")
		return
	}
	if err := c.h.retain(name, now, cfg, false); err != nil {
		c.h.log().Error("npm: retention", "package", name, "err", err)
	}
	c.audit("publish", name, v.Version, strings.Join(tags, ","), 201, "")
	c.h.log().Info("npm: published", "package", name, "version", v.Version, "token", c.tok.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": name})
}

// checkPublish validates a publish body against the URL and the tarball inside it: the name,
// version, sha512 integrity and shasum in the document must all describe the attached bytes,
// and the package.json in the tarball must carry the same name and version.
func checkPublish(name string, b publishBody, unpacked int64) (store.NPMVersion, []string, []byte, error) {
	var v store.NPMVersion
	if b.Name != name || (b.ID != "" && b.ID != name) {
		return v, nil, nil, fmt.Errorf("the document is for %q, the URL for %q", b.Name, name)
	}
	if len(b.Versions) != 1 || len(b.Attachments) != 1 {
		return v, nil, nil, errors.New("a publish carries exactly one version and one tarball; deprecation and unpublish are not supported")
	}
	var ver string
	var raw json.RawMessage
	for ver, raw = range b.Versions {
	}
	v.Name, v.Version = name, ver
	if !validVersion(ver) {
		return v, nil, nil, fmt.Errorf("version %q is not semver", ver)
	}
	var vm versionManifest
	if err := json.Unmarshal(raw, &vm); err != nil {
		return v, nil, nil, errors.New("the version object is not valid")
	}
	if vm.Name != name || vm.Version != ver {
		return v, nil, nil, fmt.Errorf("the version object is %s@%s, expected %s@%s", vm.Name, vm.Version, name, ver)
	}
	var att attachment
	for _, att = range b.Attachments {
	}
	data, err := base64.StdEncoding.DecodeString(att.Data)
	if err != nil || len(data) == 0 {
		return v, nil, nil, errors.New("the tarball is not valid base64")
	}
	if att.Length != 0 && att.Length != int64(len(data)) {
		return v, nil, nil, fmt.Errorf("the tarball is %d bytes, the document says %d", len(data), att.Length)
	}
	s512 := sha512.Sum512(data)
	s1 := sha1.Sum(data)
	if !integrityMatches(vm.Dist.Integrity, s512[:]) {
		return v, nil, nil, errors.New("dist.integrity does not match the tarball (a sha512 integrity is required)")
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(vm.Dist.Shasum)), []byte(hex.EncodeToString(s1[:]))) != 1 {
		return v, nil, nil, errors.New("dist.shasum does not match the tarball")
	}
	plugin, err := checkTarball(data, name, ver, unpacked)
	if err != nil {
		return v, nil, nil, err
	}
	v.Plugin = plugin
	var tags []string
	for t, tv := range b.DistTags {
		if !validTag(t) {
			return v, nil, nil, fmt.Errorf("invalid dist-tag %q", t)
		}
		if tv != ver {
			return v, nil, nil, fmt.Errorf("dist-tag %s points at %s, not at the published %s", t, tv, ver)
		}
		tags = append(tags, t)
	}

	// Stored without dist.tarball: the URL is built from the host of each request.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return v, nil, nil, errors.New("the version object is not valid")
	}
	delete(m, "dist")
	delete(m, "deprecated")
	manifest, _ := json.Marshal(m)
	v.Manifest = manifest
	v.SHA512 = hex.EncodeToString(s512[:])
	v.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	v.Shasum = hex.EncodeToString(s1[:])
	v.Size = int64(len(data))
	return v, tags, data, nil
}

// integrityMatches checks an SRI string: at least one sha512 entry, and every sha512 entry
// equal to sum. Entries of other algorithms are ignored.
func integrityMatches(sri string, sum []byte) bool {
	found := false
	for _, e := range strings.Fields(sri) {
		b64, ok := strings.CutPrefix(e, "sha512-")
		if !ok {
			continue
		}
		b64, _, _ = strings.Cut(b64, "?")
		got, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || subtle.ConstantTimeCompare(got, sum) != 1 {
			return false
		}
		found = true
	}
	return found
}

// checkTarball reads the tarball the way npm's extractor (node-tar with strip: 1) lays it out:
// each entry name is normalised (backslashes as separators, the first part stripped, then
// empty and "." parts dropped) and an entry with ".." is refused. Exactly one entry may become
// package.json, it must name the published package and version, and no two entries may land
// on the same path, so nothing extracted later can replace what was checked.
//
// It also tells a Claude Code plugin (docs/plugins.md): a package.json with "claude-plugin": true
// makes the package one, and then the plugin rules apply (checkPlugin). The result is nil for a
// package that is not a plugin.
func checkTarball(data []byte, name, version string, unpacked int64) (*store.NPMPlugin, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the tarball is not gzip")
	}
	tr := tar.NewReader(&capReader{r: gz, n: unpacked})
	seen := map[string]bool{}
	found := 0
	var flag json.RawMessage
	var pt pluginTar
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errTooLarge) {
			return nil, fmt.Errorf("the tarball unpacks to more than %d bytes", unpacked)
		}
		if err != nil {
			return nil, errors.New("the tarball is not a valid tar archive")
		}
		rel, err := extractedPath(hdr.Name)
		if err != nil {
			return nil, err
		}
		if rel == "" {
			continue // the top directory itself, or an entry strip: 1 drops
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if seen[rel] {
			return nil, fmt.Errorf("the tarball has %s twice", rel)
		}
		seen[rel] = true
		if err := pt.entry(rel, hdr, tr); err != nil {
			return nil, err
		}
		if rel != "package.json" {
			continue
		}
		found++
		if hdr.Typeflag != tar.TypeReg {
			return nil, errors.New("package.json in the tarball is not a regular file")
		}
		b, err := io.ReadAll(io.LimitReader(tr, 4<<20+1))
		if err != nil || len(b) > 4<<20 {
			return nil, errors.New("package.json in the tarball cannot be read")
		}
		var pj struct {
			Name         string          `json:"name"`
			Version      string          `json:"version"`
			ClaudePlugin json.RawMessage `json:"claude-plugin"`
		}
		if err := json.Unmarshal(b, &pj); err != nil {
			return nil, errors.New("package.json in the tarball is not valid JSON")
		}
		if pj.Name != name || pj.Version != version {
			return nil, fmt.Errorf("package.json in the tarball is %s@%s, the document says %s@%s", pj.Name, pj.Version, name, version)
		}
		flag = pj.ClaudePlugin
	}
	if errors.Is(pt.err, errTooLarge) {
		return nil, fmt.Errorf("the tarball unpacks to more than %d bytes", unpacked)
	}
	if found != 1 {
		return nil, errors.New("the tarball must hold exactly one package.json at its top")
	}
	return checkPlugin(name, version, flag, &pt)
}

// extractedPath is where an entry lands after extraction with strip: 1, "" when it lands
// nowhere. Like node-tar, it strips the first raw part whatever it is ("package", "." or the
// "" before a leading slash) and only then drops empty and "." parts of the rest, as the
// extractor does when it makes the path relative.
func extractedPath(name string) (string, error) {
	raw := strings.Split(strings.ReplaceAll(name, "\\", "/"), "/")
	var parts []string
	for i, p := range raw {
		switch {
		case p == "..":
			return "", fmt.Errorf("the tarball has an entry outside its directory: %q", name)
		case i == 0, p == "", p == ".":
		default:
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/"), nil
}

// writeTarball stores data under its sha512, atomically: a temp file in the same directory,
// synced, then renamed.
func (h *Handler) writeTarball(sha512hex string, data []byte) error {
	path, ok := h.tarPath(sha512hex)
	if !ok {
		return errors.New("bad hash")
	}
	return writeAtomic(path, data)
}

const tmpPrefix = ".upload-"

// distTags serves GET, PUT and DELETE of /npm/-/package/<name>/dist-tags[/<tag>].
func (c *call) distTags(name, tag string) {
	w, r := c.w, c.r
	if !c.rights.CanRead(name) {
		c.audit("tags", name, "", tag, 404, "")
		notFound(w)
		return
	}
	if r.Method == http.MethodGet && tag == "" {
		p, live, err := c.h.live(name)
		if err != nil || len(live) == 0 {
			notFound(w)
			return
		}
		c.audit("tags", name, "", "", 200, "")
		writeJSON(w, 200, liveTags(p.DistTags, live))
		return
	}
	if (r.Method != http.MethodPut && r.Method != http.MethodDelete) || tag == "" {
		notFound(w)
		return
	}
	action := "tag.set"
	if r.Method == http.MethodDelete {
		action = "tag.delete"
	}
	cfg := c.h.config()
	if !cfg.Publish {
		c.audit(action, name, "", tag, 405, "publishing is off")
		writeErr(w, http.StatusMethodNotAllowed, "publishing is off on this server (npm.publish in platform.yml)")
		return
	}
	if !c.rights.CanPublish(name) { // can read: see publish
		c.audit(action, name, "", tag, 403, "no publish right")
		writeErr(w, http.StatusForbidden, "this token cannot change the tags of "+name)
		return
	}
	if !validTag(tag) {
		writeErr(w, 400, "invalid dist-tag")
		return
	}
	var version string
	if r.Method == http.MethodPut {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&version); err != nil || !validVersion(version) {
			writeErr(w, 400, `the body must be a version as a JSON string, e.g. "1.2.3"`)
			return
		}
	} else if tag == "latest" {
		writeErr(w, 400, "the latest tag cannot be removed; point it at another version")
		return
	}

	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	status, msg := 200, ""
	err := c.h.Store.UpdateNPM(name, func(p *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		if r.Method == http.MethodDelete {
			if _, ok := p.DistTags[tag]; !ok {
				status, msg = 404, "no such tag"
				return nil, errBadRequest
			}
			delete(p.DistTags, tag)
		} else {
			ok := false
			for _, v := range vs {
				ok = ok || (v.Version == version && !v.Removed)
			}
			if !ok {
				status, msg = 404, "version "+version+" is not published"
				return nil, errBadRequest
			}
			if p.DistTags == nil {
				p.DistTags = map[string]string{}
			}
			p.DistTags[tag] = version
		}
		p.Modified = c.h.now()
		return nil, nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
		return
	case errors.Is(err, errBadRequest):
		c.audit(action, name, version, tag, status, msg)
		writeErr(w, status, msg)
		return
	case err != nil:
		writeErr(w, 500, "storage error")
		return
	}
	if err := c.h.retain(name, c.h.now(), cfg, false); err != nil {
		c.h.log().Error("npm: retention", "package", name, "err", err)
	}
	c.audit(action, name, version, tag, 200, "")
	p, live, _ := c.h.live(name)
	writeJSON(w, 200, liveTags(p.DistTags, live))
}

// retain applies retention to a package: the newest keep versions by semver and every
// dist-tagged version stay; any other version is deprecated until now+grace and, when
// deleteDue is set (the sweep), deleted once that date has passed, with its tarball and plugin
// zip unless another live version has the same tarball. A version that becomes
// kept again (tagged, or newer ones were removed) loses its deprecation. Callers hold h.mu.
func (h *Handler) retain(name string, now time.Time, cfg config.NPMConf, deleteDue bool) error {
	var gone []string
	err := h.Store.UpdateNPM(name, func(p *store.NPMPackage, vs []store.NPMVersion) ([]store.NPMVersion, error) {
		var live []store.NPMVersion
		for _, v := range vs {
			if !v.Removed {
				live = append(live, v)
			}
		}
		sortNewestFirst(live)
		keep := map[string]bool{}
		for i, v := range live {
			if i < cfg.Keep {
				keep[v.Version] = true
			}
		}
		for _, v := range p.DistTags {
			keep[v] = true
		}
		var changed []store.NPMVersion
		for _, v := range live {
			switch {
			case keep[v.Version] && !v.RemoveAt.IsZero():
				v.RemoveAt = time.Time{}
				changed = append(changed, v)
			case keep[v.Version]:
			case v.RemoveAt.IsZero():
				v.RemoveAt = now.Add(cfg.Grace.D())
				changed = append(changed, v)
			case deleteDue && !now.Before(v.RemoveAt):
				v.Removed, v.Manifest = true, nil
				changed = append(changed, v)
				gone = append(gone, v.SHA512)
				h.log().Info("npm: removed by retention", "package", name, "version", v.Version)
			}
		}
		return changed, nil
	})
	if err != nil {
		return err
	}
	for _, sum := range gone {
		used, err := h.Store.NPMHashInUse(sum)
		if err != nil || used {
			continue
		}
		if path, ok := h.tarPath(sum); ok {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				h.log().Warn("npm: remove tarball", "err", err)
			}
		}
		if path, ok := h.zipPath(sum); ok {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				h.log().Warn("npm: remove plugin zip", "err", err)
			}
		}
	}
	return nil
}
