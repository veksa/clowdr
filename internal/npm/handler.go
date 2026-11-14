// Package npm serves a private npm registry at /npm/ on the deploy host.
//
// Every route needs an npm token (auth.CheckNPM); admin and project tokens do not work here,
// and npm tokens do not work anywhere else. A token without rights on a package gets the same
// 404 as a package that does not exist, so a stranger cannot learn which packages exist.
// Versions are immutable and there is no unpublish: old versions leave only through retention
// (deprecated, then deleted after a grace period by Sweep).
package npm

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/store"
)

// Handler serves /npm/.
type Handler struct {
	Store *store.Store
	// Dir holds the registry's files: tarballs in Dir/tarballs/<sha512 hex>.
	Dir    string
	Config func() config.NPMConf
	Log    *slog.Logger
	// Now is the clock, replaced in tests.
	Now func() time.Time

	limiter limiter
	// mu serialises publishes, tag changes and the sweep: one version check and write at a time.
	mu sync.Mutex
}

const (
	mtAbbreviated = "application/vnd.npm.install-v1+json"
	// lastUsedEvery throttles writes of LastUsed: a yarn install makes hundreds of requests.
	lastUsedEvery = time.Minute
	auditKeep     = 90 * 24 * time.Hour
	auditMax      = 200_000
)

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) config() config.NPMConf {
	var c config.NPMConf
	if h.Config != nil {
		c = h.Config()
	}
	if c.Keep <= 0 {
		c.Keep = 3
	}
	if c.Grace <= 0 {
		c.Grace = config.Duration(14 * 24 * time.Hour)
	}
	if c.MaxPublish <= 0 {
		c.MaxPublish = 64 << 20
	}
	return c
}

// call is one authenticated request.
type call struct {
	h      *Handler
	w      http.ResponseWriter
	r      *http.Request
	tok    store.NPMToken
	rights auth.NPMRights
	addr   string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeErr answers in the shape npm and yarn print: {"error": "..."}. Messages never carry
// request headers, so a token cannot be echoed.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// notFound is the one answer for a package that does not exist, a name that is not valid and
// a package the token may not read.
func notFound(w http.ResponseWriter) { writeErr(w, http.StatusNotFound, "not found") }

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="clowdr"`)
	writeErr(w, http.StatusUnauthorized, "authentication required: log in with clowctl npm login")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	addr := ClientAddr(r)
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/npm/")
	segs, okSegs := splitPath(rest)
	okSegs = ok && okSegs

	// npm login and adduser: tokens come from clowctl, never from a password.
	if okSegs && r.Method == http.MethodPut && len(segs) == 3 && segs[0] == "-" && segs[1] == "user" && strings.HasPrefix(segs[2], "org.couchdb.user:") {
		msg := "this registry does not take passwords: run clowctl npm login"
		if _, err := h.authenticate(r); err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="clowdr"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg, "reason": msg})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "reason": msg})
		return
	}

	now := h.now()
	if h.limiter.blocked(addr, now) {
		w.Header().Set("Retry-After", "600")
		writeErr(w, http.StatusTooManyRequests, "too many failed authentications from this address, try again later")
		return
	}
	tok, err := h.authenticate(r)
	if err != nil {
		h.limiter.fail(addr, now)
		unauthorized(w)
		return
	}
	if now.Sub(tok.LastUsed) >= lastUsedEvery {
		if err := h.Store.TouchNPMToken(tok.ID, now); err != nil {
			h.log().Warn("npm: record token use", "token", tok.Name, "err", err)
		}
	}
	c := &call{h: h, w: w, r: r, tok: tok, rights: auth.NPMRights{Scopes: tok.Scopes}, addr: addr}
	if !okSegs || len(segs) == 0 {
		notFound(w)
		return
	}
	if segs[0] == "-" {
		c.special(segs[1:])
		return
	}
	name, sub, ok := packageName(segs)
	if !ok {
		if r.Method == http.MethodPut {
			writeErr(w, http.StatusBadRequest, "only scoped package names (@scope/name) are served here")
			return
		}
		notFound(w)
		return
	}
	switch {
	case len(sub) == 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		c.document(name)
	case len(sub) == 0 && r.Method == http.MethodPut:
		c.publish(name)
	case len(sub) == 2 && sub[0] == "-" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		c.tarball(name, sub[1])
	case r.Method == http.MethodDelete || (len(sub) > 0 && sub[0] == "-rev"):
		if !c.rights.CanRead(name) {
			notFound(w)
			return
		}
		writeErr(w, http.StatusMethodNotAllowed, "versions cannot be unpublished; old versions are deprecated and removed by retention")
	default:
		notFound(w)
	}
}

// special serves the "/npm/-/..." routes.
func (c *call) special(segs []string) {
	w, r := c.w, c.r
	route := strings.Join(segs, "/")
	switch {
	case route == "whoami" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]string{"username": c.tok.Name})
	case route == "ping" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{})
	case (route == "npm/v1/security/advisories/bulk" || route == "npm/v1/security/audits/quick") && r.Method == http.MethodPost:
		// Nothing here is in a public advisory database: an empty answer lets npm audit and
		// yarn npm audit finish instead of failing.
		io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
		writeJSON(w, 200, map[string]any{})
	case route == "clowdr/token" && r.Method == http.MethodGet:
		t := c.tok
		t.Hash = ""
		writeJSON(w, 200, t)
	case route == "clowdr/token/rotate" && r.Method == http.MethodPost:
		c.rotateSelf()
	case len(segs) >= 3 && segs[0] == "package":
		name, sub, ok := packageName(segs[1:])
		if !ok || len(sub) == 0 || sub[0] != "dist-tags" || len(sub) > 2 {
			notFound(w)
			return
		}
		tag := ""
		if len(sub) == 2 {
			tag = sub[1]
		}
		c.distTags(name, tag)
	default:
		notFound(w)
	}
}

// splitPath splits an escaped path into unescaped segments. "%2f" survives as "/" inside a
// segment, which is how npm sends "@scope%2fname". Empty and dot segments are refused.
func splitPath(escaped string) ([]string, bool) {
	if escaped == "" {
		return nil, true
	}
	parts := strings.Split(escaped, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s, err := url.PathUnescape(p)
		if err != nil || s == "" || s == "." || s == ".." {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// packageName takes "@scope/name" from the first one (from "@scope%2fname") or two segments
// and returns the segments after it.
func packageName(segs []string) (string, []string, bool) {
	if len(segs) == 0 || !strings.HasPrefix(segs[0], "@") {
		return "", nil, false
	}
	var name string
	var rest []string
	if strings.Contains(segs[0], "/") {
		name, rest = segs[0], segs[1:]
	} else if len(segs) >= 2 {
		name, rest = segs[0]+"/"+segs[1], segs[2:]
	} else {
		return "", nil, false
	}
	if !auth.ValidPackageName(name) {
		return "", nil, false
	}
	return name, rest, true
}

// tokenFromRequest reads "Authorization: Bearer <token>" or basic auth (password = token).
func tokenFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	if _, p, ok := r.BasicAuth(); ok {
		return p
	}
	return ""
}

func (h *Handler) authenticate(r *http.Request) (store.NPMToken, error) {
	return auth.CheckNPM(h.Store, tokenFromRequest(r), h.now())
}

// trustedPeer reports whether the request's X-Forwarded-* headers are believed: only when
// nginx's deploy-host location passed it (it sets them itself), or it came from the host
// itself over loopback. Anything else through the socket, or a direct TLS connection on
// :7443, cannot choose its own URLs or address.
func trustedPeer(r *http.Request) bool {
	if ingress.FromAPISocket(r) {
		return ingress.FromDeployHost(r)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ClientAddr is the address of the client: X-Forwarded-For from nginx (which has already
// applied trusted_proxies with real_ip) or the TCP peer.
func ClientAddr(r *http.Request) string {
	if trustedPeer(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if a := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(a) != nil {
				return a
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "local"
	}
	return host
}

var hostRe = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?$`)

// BaseURL is the scheme and host clients used to reach the server, for absolute tarball URLs.
func BaseURL(r *http.Request) string {
	proto, host := "http", r.Host
	if r.TLS != nil {
		proto = "https"
	}
	if trustedPeer(r) {
		if p := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); p == "http" || p == "https" {
			proto = p
		}
		if fh := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); hostRe.MatchString(fh) {
			host = fh
		}
	}
	if !hostRe.MatchString(host) {
		host = "localhost"
	}
	return proto + "://" + host
}

// document serves the package document, full or abbreviated (npm install's
// "application/vnd.npm.install-v1+json").
func (c *call) document(name string) {
	if !c.rights.CanRead(name) {
		c.audit("read", name, "", "", 404, "")
		notFound(c.w)
		return
	}
	p, live, err := c.h.live(name)
	if err != nil || len(live) == 0 {
		c.audit("read", name, "", "", 404, "")
		notFound(c.w)
		return
	}
	abbreviated := strings.Contains(c.r.Header.Get("Accept"), mtAbbreviated)
	base := BaseURL(c.r) + "/npm/"
	tags := liveTags(p.DistTags, live)
	latest := latestOf(tags, live)

	versions := map[string]json.RawMessage{}
	times := map[string]string{"created": p.Created.Format(time.RFC3339Nano), "modified": p.Modified.Format(time.RFC3339Nano)}
	var latestManifest map[string]json.RawMessage
	for _, v := range live {
		m := map[string]json.RawMessage{}
		if len(v.Manifest) > 0 {
			if err := json.Unmarshal(v.Manifest, &m); err != nil {
				c.h.log().Error("npm: stored manifest", "package", name, "version", v.Version, "err", err)
				continue
			}
		}
		if v.Version == latest {
			latestManifest = m
		}
		if abbreviated {
			for k := range m {
				if !abbreviatedFields[k] {
					delete(m, k)
				}
			}
		}
		m["name"], _ = json.Marshal(name)
		m["version"], _ = json.Marshal(v.Version)
		m["dist"], _ = json.Marshal(map[string]string{
			"integrity": v.Integrity,
			"shasum":    v.Shasum,
			"tarball":   base + name + "/-/" + tarballName(name, v.Version),
		})
		if d := deprecation(v, latest); d != "" {
			m["deprecated"], _ = json.Marshal(d)
		}
		versions[v.Version], _ = json.Marshal(m)
		times[v.Version] = v.Published.Format(time.RFC3339Nano)
	}

	var doc map[string]any
	if abbreviated {
		c.w.Header().Set("Content-Type", mtAbbreviated)
		doc = map[string]any{"name": name, "modified": p.Modified.Format(time.RFC3339Nano), "dist-tags": tags, "versions": versions}
	} else {
		doc = map[string]any{"_id": name, "name": name, "dist-tags": tags, "versions": versions, "time": times}
		for _, k := range []string{"description", "readme", "license", "homepage", "repository", "keywords", "author", "bugs"} {
			if v, ok := latestManifest[k]; ok {
				doc[k] = v
			}
		}
	}
	c.audit("read", name, "", "", 200, "")
	if c.r.Method == http.MethodHead {
		c.w.WriteHeader(200)
		return
	}
	writeJSON(c.w, 200, doc)
}

// abbreviatedFields are the version fields of an abbreviated document
// (https://github.com/npm/registry/blob/main/docs/responses/package-metadata.md).
var abbreviatedFields = map[string]bool{
	"name": true, "version": true, "deprecated": true, "dependencies": true, "optionalDependencies": true,
	"devDependencies": true, "bundleDependencies": true, "bundledDependencies": true, "peerDependencies": true,
	"peerDependenciesMeta": true, "bin": true, "directories": true, "dist": true, "engines": true,
	"_hasShrinkwrap": true, "hasInstallScript": true, "cpu": true, "os": true, "libc": true, "funding": true,
	"acceptDependencies": true,
}

// live returns the package and its versions that are not removed, newest first.
func (h *Handler) live(name string) (store.NPMPackage, []store.NPMVersion, error) {
	p, err := h.Store.NPMPackage(name)
	if err != nil {
		return p, nil, err
	}
	vs, err := h.Store.NPMVersions(name)
	if err != nil {
		return p, nil, err
	}
	live := vs[:0]
	for _, v := range vs {
		if !v.Removed {
			live = append(live, v)
		}
	}
	sortNewestFirst(live)
	return p, live, nil
}

func sortNewestFirst(vs []store.NPMVersion) {
	sort.SliceStable(vs, func(i, j int) bool { return compareSemver(vs[i].Version, vs[j].Version) > 0 })
}

// liveTags drops tags that point at versions no longer served.
func liveTags(tags map[string]string, live []store.NPMVersion) map[string]string {
	have := map[string]bool{}
	for _, v := range live {
		have[v.Version] = true
	}
	out := map[string]string{}
	for t, v := range tags {
		if have[v] {
			out[t] = v
		}
	}
	return out
}

// latestOf is the "latest" tag, or the highest version without one. live is newest first.
func latestOf(tags map[string]string, live []store.NPMVersion) string {
	if l := tags["latest"]; l != "" {
		return l
	}
	if len(live) > 0 {
		return live[0].Version
	}
	return ""
}

// deprecation is the message retention puts on a version it is about to delete.
func deprecation(v store.NPMVersion, latest string) string {
	if v.RemoveAt.IsZero() {
		return ""
	}
	return "removed on " + v.RemoveAt.Format("2006-01-02") + ", upgrade to " + latest
}

// tarballName is the file name npm uses: "name-1.2.3.tgz" for "@scope/name".
func tarballName(name, version string) string {
	_, base, _ := strings.Cut(name, "/")
	return base + "-" + version + ".tgz"
}

var hexRe = regexp.MustCompile(`^[0-9a-f]{128}$`)

func (h *Handler) tarPath(sha512hex string) (string, bool) {
	if !hexRe.MatchString(sha512hex) {
		return "", false
	}
	return filepath.Join(h.Dir, "tarballs", sha512hex), true
}

func (c *call) tarball(name, file string) {
	if !c.rights.CanRead(name) {
		c.audit("download", name, "", "", 404, "")
		notFound(c.w)
		return
	}
	_, base, _ := strings.Cut(name, "/")
	ver, ok := strings.CutPrefix(file, base+"-")
	ver, ok2 := strings.CutSuffix(ver, ".tgz")
	if !ok || !ok2 || !validVersion(ver) {
		c.audit("download", name, "", "", 404, "")
		notFound(c.w)
		return
	}
	v, err := c.h.Store.NPMVersion(name, ver)
	path, okPath := c.h.tarPath(v.SHA512)
	if err != nil || v.Removed || !okPath {
		c.audit("download", name, ver, "", 404, "")
		notFound(c.w)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		c.h.log().Error("npm: tarball missing", "package", name, "version", ver, "err", err)
		c.audit("download", name, ver, "", 404, "file missing")
		notFound(c.w)
		return
	}
	defer f.Close()
	c.audit("download", name, ver, "", 200, "")
	c.w.Header().Set("Content-Type", "application/octet-stream")
	// The bytes behind a version's URL never change; only the client that fetched them keeps them.
	c.w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(c.w, c.r, "", v.Published, f)
}

// audit records an action on the registry. A failure to record is logged, not fatal: the
// request has been decided already.
func (c *call) audit(action, pkg, version, tag string, status int, detail string) {
	c.h.record(store.NPMAudit{
		Time: c.h.now(), Action: action, Token: c.tok.Name, TokenID: c.tok.ID, Addr: c.addr,
		Package: pkg, Version: version, Tag: tag, Status: status, Detail: detail,
	})
}

func (h *Handler) record(e store.NPMAudit) {
	if err := h.Store.AddNPMAudit(e); err != nil {
		h.log().Error("npm: audit", "action", e.Action, "err", err)
	}
}

// limiter counts failed authentications per client address: after failMax failures within
// failWindow the address gets 429 until the window ends, even with a valid token.
type limiter struct {
	mu sync.Mutex
	m  map[string]*failures
}

type failures struct {
	n     int
	since time.Time
}

const (
	failMax    = 20
	failWindow = 10 * time.Minute
)

func (l *limiter) blocked(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.m[addr]
	if f == nil {
		return false
	}
	if now.Sub(f.since) > failWindow {
		delete(l.m, addr)
		return false
	}
	return f.n >= failMax
}

func (l *limiter) fail(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*failures{}
	}
	f := l.m[addr]
	if f == nil || now.Sub(f.since) > failWindow {
		if len(l.m) >= 10_000 {
			l.pruneLocked(now)
		}
		f = &failures{since: now}
		l.m[addr] = f
	}
	f.n++
}

func (l *limiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
}

func (l *limiter) pruneLocked(now time.Time) {
	for a, f := range l.m {
		if now.Sub(f.since) > failWindow {
			delete(l.m, a)
		}
	}
}

var errBadRequest = errors.New("bad request")
