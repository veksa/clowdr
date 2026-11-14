package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/npm"
	"github.com/veksa/clowdr/internal/store"
)

// npmMinServer is the first clowdrd with the npm registry.
const npmMinServer = "v0.4.0"

var errNPMTooOld = fmt.Errorf("server too old for npm (needs clowdrd %s or newer: clowdr self-update on the server)", npmMinServer)

// NPMConfig is the npm section of ~/.clowdr/config: the tokens `clowctl npm login` wrote into
// ~/.yarnrc.yml and ~/.npmrc, kept here for expiry warnings, rotation and checks.
type NPMConfig struct {
	Logins []*NPMLogin `yaml:"logins,omitempty"`
}

type NPMLogin struct {
	Registry string    `yaml:"registry"` // https://deploy.example.com/npm/
	Context  string    `yaml:"context,omitempty"`
	Name     string    `yaml:"name"`
	Token    string    `yaml:"token"`
	Expires  time.Time `yaml:"expires"`
	// Scopes are the package scopes ("@acme") the token is for; checks guard them against
	// dependency confusion.
	Scopes []string `yaml:"scopes,omitempty"`
}

func (c *UserConfig) npmLogin(registry string) *NPMLogin {
	if c.NPM == nil {
		return nil
	}
	for _, l := range c.NPM.Logins {
		if l.Registry == registry {
			return l
		}
	}
	return nil
}

func (c *UserConfig) setNPMLogin(l *NPMLogin) {
	if c.NPM == nil {
		c.NPM = &NPMConfig{}
	}
	for i, o := range c.NPM.Logins {
		if o.Registry == l.Registry {
			c.NPM.Logins[i] = l
			return
		}
	}
	c.NPM.Logins = append(c.NPM.Logins, l)
}

// registryURL is the npm registry of a clowdr server: <server>/npm/.
func registryURL(server string) string { return strings.TrimRight(server, "/") + "/npm/" }

// normRegistry gives a registry URL its trailing slash.
func normRegistry(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	return strings.TrimRight(u, "/") + "/"
}

// registryServer is the clowdr server of a registry URL.
func registryServer(reg string) string {
	return strings.TrimSuffix(strings.TrimRight(reg, "/"), "/npm")
}

// checkRegistryTransport refuses to send a token over plain http, except to this machine.
func checkRegistryTransport(reg string) error {
	u, err := url.Parse(reg)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: not a URL", reg)
	}
	if u.Scheme != "https" && !isLocalHost(u.Hostname()) {
		return fmt.Errorf("%s: use https (plain http only to localhost)", reg)
	}
	return nil
}

func isLocalHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// npmrcAuthKey is the .npmrc key holding the token of a registry: "//host/npm/:_authToken".
func npmrcAuthKey(reg string) string {
	u, _ := url.Parse(reg)
	return "//" + u.Host + u.Path + ":_authToken"
}

func homeFile(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, name), nil
}

// writeHomeTokens puts the token where yarn (~/.yarnrc.yml, merged with the project's file) and
// npm (~/.npmrc) look for it, both with mode 0600.
func writeHomeTokens(reg, token string) ([]string, error) {
	yrc, err := homeFile(".yarnrc.yml")
	if err != nil {
		return nil, err
	}
	nrc, err := homeFile(".npmrc")
	if err != nil {
		return nil, err
	}
	if err := yamlSet(yrc, token, true, "npmRegistries", reg, "npmAuthToken"); err != nil {
		return nil, err
	}
	if err := npmrcSet(nrc, npmrcAuthKey(reg), token); err != nil {
		return nil, err
	}
	return []string{yrc, nrc}, nil
}

// needNPMServer fails with errNPMTooOld when the server does not list the npm feature.
func needNPMServer(ctx context.Context, c *api.Client) error {
	var v api.Version
	if err := c.Do(ctx, "GET", "/v1/version", nil, &v); err != nil {
		return err
	}
	if !slices.Contains(v.Features, api.FeatureNPM) {
		return errNPMTooOld
	}
	return nil
}

// npmTokenClient talks to a registry with an npm token. An old server has no /npm/ at all,
// which whoami tells apart from a refused token.
func (a *app) npmTokenClient(ctx context.Context, l *NPMLogin) (*api.Client, error) {
	if err := checkRegistryTransport(l.Registry); err != nil {
		return nil, err
	}
	fp := ""
	if c := a.cfg.Contexts[l.Context]; c != nil && strings.TrimRight(c.Server, "/") == registryServer(l.Registry) {
		fp = c.Fingerprint
	} else if c := a.cfg.byServer(registryServer(l.Registry)); c != nil {
		fp = c.Fingerprint
	}
	c := api.NewClient(registryServer(l.Registry), l.Token, fp)
	var who map[string]string
	if err := c.Do(ctx, "GET", "/npm/-/whoami", nil, &who); err != nil {
		var se *api.StatusError
		switch {
		case errors.As(err, &se) && se.Status == 404:
			return nil, errNPMTooOld
		case errors.As(err, &se) && se.Status == 401:
			return nil, fmt.Errorf("the npm token %s for %s is not accepted (expired or revoked): clowctl npm login", l.Name, l.Registry)
		}
		return nil, err
	}
	return c, nil
}

// npmRegistryFor picks the registry an npm-token command works with: --context's server, else
// the current context's, else the only login.
func (a *app) npmRegistryFor() (string, error) {
	name := a.contextName
	if name == "" {
		name = a.cfg.Current
	}
	if c := a.cfg.Contexts[name]; c != nil {
		return registryURL(c.Server), nil
	}
	if a.cfg.NPM != nil && len(a.cfg.NPM.Logins) == 1 {
		return a.cfg.NPM.Logins[0].Registry, nil
	}
	return "", errors.New("no registry: pass --context")
}

func (a *app) npmLoginFor() (*NPMLogin, error) {
	reg, err := a.npmRegistryFor()
	if err != nil {
		return nil, err
	}
	l := a.cfg.npmLogin(reg)
	if l == nil {
		return nil, fmt.Errorf("no npm token for %s: clowctl npm login --context <admin context>", reg)
	}
	return l, nil
}

// warnNPMExpiry warns about local npm tokens that expire within 30 days. It reads only the
// local config: no request is made.
func (a *app) warnNPMExpiry(now time.Time) {
	if a.cfg == nil || a.cfg.NPM == nil {
		return
	}
	for _, l := range a.cfg.NPM.Logins {
		left := l.Expires.Sub(now)
		switch {
		case l.Expires.IsZero() || left > 30*24*time.Hour:
		case left <= 0:
			fmt.Fprintf(a.err, "! npm token %s for %s has expired: clowctl npm token rotate %s --context <admin context>\n", l.Name, l.Registry, l.Name)
		default:
			fmt.Fprintf(a.err, "! npm token %s for %s expires in %d days (%s): clowctl npm token rotate %s --context <admin context>\n",
				l.Name, l.Registry, int(left.Hours()/24), l.Expires.Format("2006-01-02"), l.Name)
		}
	}
}

var tokenNameClean = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

func defaultTokenName() string {
	h, _ := os.Hostname()
	h = strings.Trim(tokenNameClean.ReplaceAllString(h, "-"), "-.")
	if h == "" {
		h = "machine"
	}
	if len(h) > 64 {
		h = h[:64]
	}
	return h
}

// tokenScopes turns --scope values into token scopes: "@acme" or "@acme/*" reads the whole
// scope, "@acme/tool" one package, and a full "npm:...", "plugins:read" passes as it is.
func tokenScopes(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		switch {
		case strings.HasPrefix(s, "npm:") || strings.HasPrefix(s, "plugins:"):
		case strings.HasPrefix(s, "@") && !strings.Contains(s, "/"):
			s = "npm:read:" + s + "/*"
		default:
			s = "npm:read:" + s
		}
		if err := auth.ValidScope(s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// packageScopes are the "@scope" parts of token scopes.
func packageScopes(scopes []string) []string {
	var out []string
	for _, s := range scopes {
		for _, k := range []string{auth.ScopeNPMRead + ":", auth.ScopeNPMPublish + ":"} {
			if p, ok := strings.CutPrefix(s, k); ok {
				if sc := scopeOf(p); sc != "" && !slices.Contains(out, sc) {
					out = append(out, sc)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// parseExpiry reads "1y", "90d", "720h".
func parseExpiry(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "y"); ok {
		years, err := strconv.Atoi(n)
		if err != nil || years <= 0 {
			return 0, fmt.Errorf("--expires: invalid %q", s)
		}
		return time.Duration(years) * 365 * 24 * time.Hour, nil
	}
	d, err := config.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--expires: invalid %q (e.g. 1y, 90d)", s)
	}
	return d, nil
}

func (a *app) npmCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "npm", Short: "Private npm registry: login, project setup, publish, checks, tokens"}
	cmd.AddCommand(a.npmLoginCmd(), a.npmSetupCmd(), a.npmPublishCmd(), a.npmCheckCmd(), a.npmAuditCmd(), a.npmTokenCmd())
	return cmd
}

func (a *app) npmLoginCmd() *cobra.Command {
	var name, expires string
	var scopes []string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Create an npm token for this machine and write it to ~/.yarnrc.yml and ~/.npmrc",
		Long: `Creates an npm token with an admin context (--context) and writes it, mode 0600, to
~/.yarnrc.yml (npmRegistries."<server>/npm/".npmAuthToken) and ~/.npmrc, where yarn and npm
find it for every project. The token's name, expiry and scopes are kept in ~/.clowdr/config.

--scope @acme reads every package of the scope, @acme/tool one package; a full scope such as
npm:publish:@acme/* or plugins:read is passed as it is. Running it again with a token that still
works only rewrites the files.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, err := a.target(false)
			if err != nil {
				return err
			}
			reg := registryURL(t.client.Server)
			if err := checkRegistryTransport(reg); err != nil {
				return err
			}
			if err := needNPMServer(ctx, t.client); err != nil {
				return err
			}
			if name == "" {
				name = defaultTokenName()
			}
			ctxName := a.contextName
			if ctxName == "" {
				ctxName = a.cfg.Current
			}
			if l := a.cfg.npmLogin(reg); l != nil && l.Name == name && l.Token != "" && time.Now().Before(l.Expires) {
				if _, err := a.npmTokenClient(ctx, l); err == nil {
					files, err := writeHomeTokens(reg, l.Token)
					if err != nil {
						return err
					}
					fmt.Fprintf(a.err, "✓ already logged in to %s as %s (expires %s); wrote %s\n", reg, name, l.Expires.Format("2006-01-02"), strings.Join(files, ", "))
					return nil
				}
			}
			if len(scopes) == 0 {
				return errors.New("--scope is required, e.g. --scope @acme")
			}
			ts, err := tokenScopes(scopes)
			if err != nil {
				return err
			}
			ttl, err := parseExpiry(expires)
			if err != nil {
				return err
			}
			var tr npm.TokenResponse
			err = t.client.Do(ctx, "POST", "/v1/npm/tokens", api.NPMTokenRequest{Name: name, Scopes: ts, ExpiresIn: fmt.Sprintf("%dh", int(ttl.Hours()))}, &tr)
			var se *api.StatusError
			if errors.As(err, &se) && se.Status == 409 {
				return fmt.Errorf("a token named %s exists on the server: pick another --name, or revoke it first (clowctl npm token revoke %s)", name, name)
			} else if err != nil {
				return err
			}
			files, err := writeHomeTokens(reg, tr.Token)
			if err != nil {
				return err
			}
			a.cfg.setNPMLogin(&NPMLogin{Registry: reg, Context: ctxName, Name: name, Token: tr.Token, Expires: tr.Info.Expires, Scopes: packageScopes(ts)})
			if err := a.cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(a.err, "✓ logged in to %s as %s (%s), expires %s; wrote %s\n", reg, name, strings.Join(ts, " "), tr.Info.Expires.Format("2006-01-02"), strings.Join(files, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "token name (default: this machine's host name)")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "what the token reads: @scope, @scope/name, or a full token scope (repeatable)")
	cmd.Flags().StringVar(&expires, "expires", "1y", "token lifetime, e.g. 1y, 90d")
	return cmd
}

// findUp returns the nearest path named name in dir or its parents, skipping skip.
func findUp(dir, name, skip string) string {
	dir, _ = filepath.Abs(dir)
	for {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil && p != skip {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// projectYarnrc is the project's .yarnrc.yml for dir: the nearest one above dir (never the one
// in the home directory, which holds tokens), else a new one next to the nearest package.json.
func projectYarnrc(dir string) (string, error) {
	home, _ := homeFile(".yarnrc.yml")
	if p := findUp(dir, ".yarnrc.yml", home); p != "" {
		return p, nil
	}
	if p := findUp(dir, "package.json", ""); p != "" {
		return filepath.Join(filepath.Dir(p), ".yarnrc.yml"), nil
	}
	return "", errors.New("no .yarnrc.yml or package.json here or above: run this in a project")
}

func (a *app) npmSetupCmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Map package scopes to this registry in the project's .yarnrc.yml",
		Long: `Writes npmScopes.<scope>.npmRegistryServer into the project's .yarnrc.yml: the nearest
.yarnrc.yml in this directory or above (the one in your home directory is never used), or a new
one next to the nearest package.json. Commit the file: it is what sends the scope to this
registry instead of the public one. The token stays in ~/.yarnrc.yml (clowctl npm login).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(scopes) == 0 {
				return errors.New("--scope is required, e.g. --scope @acme")
			}
			reg, err := a.npmRegistryFor()
			if err != nil {
				return err
			}
			if err := checkRegistryTransport(reg); err != nil {
				return err
			}
			file, err := projectYarnrc(".")
			if err != nil {
				return err
			}
			for _, s := range scopes {
				if !regexp.MustCompile(`^@[a-z0-9][a-z0-9._~-]*$`).MatchString(s) {
					return fmt.Errorf("--scope %q: want @scope", s)
				}
				if err := yamlSet(file, reg, false, "npmScopes", strings.TrimPrefix(s, "@"), "npmRegistryServer"); err != nil {
					return err
				}
				if l := a.cfg.npmLogin(reg); l != nil && !slices.Contains(l.Scopes, s) {
					l.Scopes = append(l.Scopes, s)
					sort.Strings(l.Scopes)
				}
				fmt.Fprintf(a.err, "✓ %s → %s in %s\n", s, reg, file)
			}
			return a.cfg.Save()
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "package scope to map, e.g. @acme (repeatable)")
	return cmd
}

// packTarball packs the package in dir with the project's own tool: yarn when the project uses
// it (a yarn.lock here or above), npm otherwise. Replaced in tests.
var packTarball = func(ctx context.Context, dir string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "clowctl-pack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	var c *exec.Cmd
	if findUp(dir, "yarn.lock", "") != "" {
		c = exec.CommandContext(ctx, "yarn", "pack", "--out", filepath.Join(tmp, "package.tgz"))
	} else {
		c = exec.CommandContext(ctx, "npm", "pack", "--pack-destination", tmp)
	}
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: %v\n%s", strings.Join(c.Args, " "), err, out)
	}
	es, err := os.ReadDir(tmp)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".tgz") {
			return os.ReadFile(filepath.Join(tmp, e.Name()))
		}
	}
	return nil, errors.New("pack produced no .tgz")
}

// tarballManifest reads package.json from the top directory of an npm tarball.
func tarballManifest(tgz []byte) (map[string]json.RawMessage, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, errors.New("the packed tarball is not gzip")
	}
	tr := tar.NewReader(io.LimitReader(gz, 512<<20))
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("the packed tarball has no package.json")
		}
		dir, file, ok := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		if ok && dir != "" && file == "package.json" {
			b, err := io.ReadAll(io.LimitReader(tr, 4<<20))
			if err != nil {
				return nil, err
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, fmt.Errorf("package.json in the tarball: %w", err)
			}
			return m, nil
		}
	}
}

func rawString(m map[string]json.RawMessage, k string) string {
	var s string
	json.Unmarshal(m[k], &s)
	return s
}

// buildPublishBody makes the document `npm publish` sends: the version object is the packed
// package.json with _id and dist (sha512 integrity, sha1 shasum, tarball URL) added, and the
// tarball travels base64-encoded in _attachments.
func buildPublishBody(reg string, tgz []byte, tag string) (body []byte, name, version string, err error) {
	m, err := tarballManifest(tgz)
	if err != nil {
		return nil, "", "", err
	}
	name, version = rawString(m, "name"), rawString(m, "version")
	if !auth.ValidPackageName(name) {
		return nil, "", "", fmt.Errorf("package name %q: only scoped names (@scope/name) are published here", name)
	}
	if version == "" {
		return nil, "", "", errors.New("package.json has no version")
	}
	if tag == "" {
		tag = "latest"
	}
	s512, s1 := sha512.Sum512(tgz), sha1.Sum(tgz)
	_, base, _ := strings.Cut(name, "/")
	v := map[string]any{}
	for k, raw := range m {
		v[k] = raw
	}
	v["_id"] = name + "@" + version
	v["dist"] = map[string]string{
		"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
		"shasum":    hex.EncodeToString(s1[:]),
		"tarball":   reg + name + "/-/" + base + "-" + version + ".tgz",
	}
	doc := map[string]any{
		"_id": name, "name": name, "description": json.RawMessage(orNull(m["description"])),
		"dist-tags": map[string]string{tag: version},
		"versions":  map[string]any{version: v},
		"access":    "restricted",
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString(tgz),
			"length":       len(tgz),
		}},
	}
	body, err = json.Marshal(doc)
	return body, name, version, err
}

func orNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func (a *app) npmPublishCmd() *cobra.Command {
	var tag string
	cmd := &cobra.Command{
		Use:   "publish [dir]",
		Short: "Pack the package with the project's own npm or yarn and publish it here",
		Long: `Packs the package in dir (default: here) with yarn pack (in a yarn project) or npm pack and
publishes the tarball with the npm token from clowctl npm login. package.json must name a scoped
package and set publishConfig.registry to this registry, so that a plain npm publish can never
send it to the public registry instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			b, err := os.ReadFile(filepath.Join(dir, "package.json"))
			if err != nil {
				return err
			}
			var pj struct {
				Name          string `json:"name"`
				PublishConfig struct {
					Registry string `json:"registry"`
				} `json:"publishConfig"`
			}
			if err := json.Unmarshal(b, &pj); err != nil {
				return fmt.Errorf("package.json: %w", err)
			}
			if !auth.ValidPackageName(pj.Name) {
				return fmt.Errorf("package name %q: only scoped names (@scope/name) are published here", pj.Name)
			}
			reg := normRegistry(pj.PublishConfig.Registry)
			if reg == "" {
				return errors.New(`package.json has no publishConfig.registry: set it to this registry, e.g. "publishConfig": {"registry": "https://deploy.example.com/npm/"}`)
			}
			l := a.cfg.npmLogin(reg)
			if l == nil {
				known, _ := a.npmRegistryFor()
				return fmt.Errorf("publishConfig.registry %s is not a clowdr registry you are logged in to (this context's is %s): clowctl npm login", reg, known)
			}
			c, err := a.npmTokenClient(ctx, l)
			if err != nil {
				return err
			}
			tgz, err := packTarball(ctx, dir)
			if err != nil {
				return err
			}
			body, name, version, err := buildPublishBody(reg, tgz, tag)
			if err != nil {
				return err
			}
			if name != pj.Name {
				return fmt.Errorf("the packed package is %s, package.json says %s", name, pj.Name)
			}
			if err := c.Do(ctx, "PUT", "/npm/"+strings.Replace(name, "/", "%2f", 1), json.RawMessage(body), nil); err != nil {
				return fmt.Errorf("publish %s@%s: %w", name, version, err)
			}
			if tag == "" {
				tag = "latest"
			}
			fmt.Fprintf(a.err, "✓ published %s@%s (%s) to %s\n", name, version, tag, reg)
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "latest", "dist-tag to point at the new version")
	return cmd
}

// npmProblem is one finding of a check.
type npmProblem struct {
	Package string
	Message string
}

// npmCheckResult is what a check found; Guarded is false when the project uses no package scope
// tied to a clowdr registry, so there was nothing to check.
type npmCheckResult struct {
	Guarded  bool
	Problems []npmProblem
}

// clowdrRegistries are the registries known to be clowdr's: those logged in to and those of
// every context.
func (a *app) clowdrRegistries() map[string]bool {
	regs := map[string]bool{}
	if a.cfg.NPM != nil {
		for _, l := range a.cfg.NPM.Logins {
			regs[l.Registry] = true
		}
	}
	for _, c := range a.cfg.Contexts {
		regs[registryURL(c.Server)] = true
	}
	return regs
}

// npmCheck checks the project at dir:
//   - every package of a guarded scope (one a login is for, or one the project maps to a clowdr
//     registry) that the project depends on or locks must be mapped to that registry in the
//     project's .yarnrc.yml or .npmrc, or a machine without the mapping would ask the public
//     registry for it (dependency confusion);
//   - every locked version of such a package must still be served and not deprecated by
//     retention.
func (a *app) npmCheck(ctx context.Context, dir string) (npmCheckResult, error) {
	var res npmCheckResult
	homeYarnrc, _ := homeFile(".yarnrc.yml")
	homeNpmrc, _ := homeFile(".npmrc")
	mapped := map[string]string{} // @scope → registry
	yarnrc := findUp(dir, ".yarnrc.yml", homeYarnrc)
	if yarnrc != "" {
		scopes, err := yamlMap(yarnrc, "npmScopes")
		if err != nil {
			return res, err
		}
		for s, n := range scopes {
			if v, err := yamlChild(n, "npmRegistryServer", false); err == nil && v != nil {
				mapped["@"+s] = normRegistry(v.Value)
			}
		}
	}
	if npmrc := findUp(dir, ".npmrc", homeNpmrc); npmrc != "" {
		b, _ := os.ReadFile(npmrc)
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if s, isScope := strings.CutSuffix(strings.TrimSpace(k), ":registry"); ok && isScope && strings.HasPrefix(s, "@") {
				if _, dup := mapped[s]; !dup {
					mapped[s] = normRegistry(v)
				}
			}
		}
	}
	clowdr := a.clowdrRegistries()
	guarded := map[string]bool{}
	if a.cfg.NPM != nil {
		for _, l := range a.cfg.NPM.Logins {
			for _, s := range l.Scopes {
				guarded[s] = true
			}
		}
	}
	for s, reg := range mapped {
		if clowdr[reg] {
			guarded[s] = true
		}
	}

	var locked []lockedPackage
	lockDir := ""
	for _, f := range []string{"yarn.lock", "package-lock.json"} {
		if p := findUp(dir, f, ""); p != "" && (lockDir == "" || len(filepath.Dir(p)) > len(lockDir)) {
			lockDir = filepath.Dir(p)
		}
	}
	if lockDir != "" {
		var err error
		if locked, err = lockedPackages(lockDir); err != nil {
			return res, err
		}
	}
	deps, err := manifestDeps(dir)
	if err != nil {
		return res, err
	}
	used := map[string]bool{}
	for _, d := range deps {
		used[d] = true
	}
	for _, p := range locked {
		used[p.Name] = true
	}
	names := make([]string, 0, len(used))
	for n := range used {
		if guarded[scopeOf(n)] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return res, nil
	}
	res.Guarded = true
	where := yarnrc
	if where == "" {
		where = ".yarnrc.yml"
	}
	badScope := map[string]bool{}
	for _, n := range names {
		s := scopeOf(n)
		switch reg := mapped[s]; {
		case badScope[s]:
		case reg == "":
			badScope[s] = true
			res.Problems = append(res.Problems, npmProblem{n, fmt.Sprintf("%s is used but %s does not map it to the clowdr registry, so it could be fetched from the public registry: clowctl npm setup --scope %s", s, where, s)})
		case !clowdr[reg]:
			badScope[s] = true
			res.Problems = append(res.Problems, npmProblem{n, fmt.Sprintf("%s is mapped to %s, which is not a clowdr registry you know", s, reg)})
		}
	}

	docs := map[string]*registryDoc{}
	clients := map[string]*api.Client{}
	for _, p := range locked {
		s := scopeOf(p.Name)
		if !guarded[s] || badScope[s] {
			continue
		}
		reg := mapped[s]
		c, ok := clients[reg]
		if !ok {
			l := a.cfg.npmLogin(reg)
			if l == nil {
				tok, _ := yamlGet(homeYarnrc, "npmRegistries", reg, "npmAuthToken")
				if tok == "" {
					return res, fmt.Errorf("no npm token for %s to check %s: clowctl npm login", reg, p.Name)
				}
				l = &NPMLogin{Registry: reg, Token: tok, Name: "(from ~/.yarnrc.yml)"}
			}
			if c, err = a.npmTokenClient(ctx, l); err != nil {
				return res, err
			}
			clients[reg] = c
		}
		doc, ok := docs[p.Name]
		if !ok {
			doc = &registryDoc{}
			err := c.Do(ctx, "GET", "/npm/"+strings.Replace(p.Name, "/", "%2f", 1), nil, doc)
			var se *api.StatusError
			if errors.As(err, &se) && se.Status == 404 {
				doc = nil
			} else if err != nil {
				return res, fmt.Errorf("%s: %w", p.Name, err)
			}
			docs[p.Name] = doc
		}
		switch {
		case doc == nil:
			res.Problems = append(res.Problems, npmProblem{p.Name, fmt.Sprintf("%s@%s is not in %s (removed, or your token cannot read it): yarn up %s", p.Name, p.Version, reg, p.Name)})
		case doc.Versions[p.Version] == nil:
			res.Problems = append(res.Problems, npmProblem{p.Name, fmt.Sprintf("%s@%s was removed by retention (latest is %s): yarn up %s", p.Name, p.Version, doc.DistTags["latest"], p.Name)})
		case doc.Versions[p.Version].Deprecated != "":
			res.Problems = append(res.Problems, npmProblem{p.Name, fmt.Sprintf("%s@%s is deprecated (%s): yarn up %s", p.Name, p.Version, doc.Versions[p.Version].Deprecated, p.Name)})
		}
	}
	return res, nil
}

type registryDoc struct {
	DistTags map[string]string `json:"dist-tags"`
	Versions map[string]*struct {
		Deprecated string `json:"deprecated"`
	} `json:"versions"`
}

func (a *app) printProblems(ps []npmProblem) {
	for _, p := range ps {
		fmt.Fprintf(a.err, "✗ %s\n", p.Message)
	}
}

func (a *app) npmCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check [dir]",
		Short: "Check locked packages from clowdr registries: none deprecated or removed, every scope mapped",
		Long: `Reads yarn.lock (yarn 2+) and package-lock.json and package.json for the project in dir
(default: here). For every package scope tied to a clowdr registry (a scope your npm login is
for, or one the project maps to a clowdr registry) it fails when:
  - the scope is not mapped in the project's .yarnrc.yml or .npmrc, so a machine without the
    mapping would fetch it from the public registry;
  - a locked version is deprecated by retention or already removed (yarn up <package>).
clowctl deploy runs the same check first; --allow-outdated skips it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			res, err := a.npmCheck(cmd.Context(), dir)
			if err != nil {
				return err
			}
			if !res.Guarded {
				fmt.Fprintln(a.err, "no packages from clowdr registries")
				return nil
			}
			if len(res.Problems) > 0 {
				a.printProblems(res.Problems)
				return fmt.Errorf("%d problem(s) with packages from clowdr registries", len(res.Problems))
			}
			fmt.Fprintln(a.err, "✓ packages from clowdr registries are current")
			return nil
		},
	}
}

// npmCheckDeploy runs the check for deploy.yml's directory and every service directory.
func (a *app) npmCheckDeploy(ctx context.Context, d *config.Deploy) error {
	dirs := []string{d.Dir}
	for _, n := range d.ServiceNames() {
		p := filepath.Join(d.Dir, filepath.FromSlash(d.Services[n].Path))
		if !slices.Contains(dirs, p) {
			dirs = append(dirs, p)
		}
	}
	var problems []npmProblem
	seen := map[string]bool{}
	for _, dir := range dirs {
		res, err := a.npmCheck(ctx, dir)
		if err != nil {
			return fmt.Errorf("checking packages from clowdr registries in %s: %w (deploy anyway: --allow-outdated)", dir, err)
		}
		for _, p := range res.Problems {
			if !seen[p.Message] {
				seen[p.Message] = true
				problems = append(problems, p)
			}
		}
	}
	if len(problems) > 0 {
		a.printProblems(problems)
		return fmt.Errorf("%d problem(s) with packages from clowdr registries: fix them, or deploy anyway with --allow-outdated", len(problems))
	}
	return nil
}

func (a *app) npmAuditCmd() *cobra.Command {
	var pkg, token, since, action string
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the registry's audit log (admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			if err := needNPMServer(cmd.Context(), t.client); err != nil {
				return err
			}
			q := url.Values{}
			for k, v := range map[string]string{"package": pkg, "token": token, "since": since, "action": action} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var es []store.NPMAudit
			if err := t.client.Do(cmd.Context(), "GET", "/v1/npm/audit?"+q.Encode(), nil, &es); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(es)
			}
			w := a.table("TIME", "ACTION", "TOKEN", "ADDRESS", "PACKAGE", "VERSION", "TAG", "STATUS", "DETAIL")
			for _, e := range es {
				st := "-"
				if e.Status != 0 {
					st = strconv.Itoa(e.Status)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Action,
					orDash(e.Token), orDash(e.Addr), orDash(e.Package), orDash(e.Version), orDash(e.Tag), st, orDash(e.Detail))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&pkg, "package", "", "only this package, e.g. @acme/tool")
	cmd.Flags().StringVar(&token, "token", "", "only this token name")
	cmd.Flags().StringVar(&action, "action", "", "only this action (read, download, publish, tag, token, ...)")
	cmd.Flags().StringVar(&since, "since", "", "only entries newer than this, e.g. 24h, 7d")
	cmd.Flags().IntVar(&limit, "limit", 100, "at most this many entries")
	return cmd
}

func (a *app) npmTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "npm tokens: list, revoke, rotate"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List npm tokens (admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			if err := needNPMServer(cmd.Context(), t.client); err != nil {
				return err
			}
			var ts []store.NPMToken
			if err := t.client.Do(cmd.Context(), "GET", "/v1/npm/tokens", nil, &ts); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(ts)
			}
			w := a.table("NAME", "ID", "SCOPES", "EXPIRES", "LAST USED", "STATE")
			now := time.Now()
			for _, tk := range ts {
				state := "active"
				switch {
				case !now.Before(tk.Expires):
					state = "expired"
				case tk.ReplacedBy != "":
					state = "rotated, works until " + tk.Expires.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", tk.Name, tk.ID, strings.Join(tk.Scopes, " "), tk.Expires.Local().Format("2006-01-02"), ago(tk.LastUsed), state)
			}
			return w.Flush()
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke an npm token, including one still in a rotation's grace period (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			if err := needNPMServer(cmd.Context(), t.client); err != nil {
				return err
			}
			if err := t.client.Do(cmd.Context(), "DELETE", "/v1/npm/tokens/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.err, "✓ npm token %s revoked\n", args[0])
			return nil
		},
	}
	var now bool
	rotate := &cobra.Command{
		Use:   "rotate [name]",
		Short: "Replace an npm token; the old one works 24 h more (--now: stops at once)",
		Long: `Without a name, rotates this machine's own token (from clowctl npm login) and rewrites
~/.yarnrc.yml and ~/.npmrc; the new token keeps the old one's expiry. With a name and an admin
context, rotates that token and renews its full lifetime; when it is this machine's token the
files are rewritten too, otherwise the new token is printed once for its owner.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var tr npm.TokenResponse
			var reg string
			if len(args) == 0 {
				l, err := a.npmLoginFor()
				if err != nil {
					return err
				}
				c, err := a.npmTokenClient(ctx, l)
				if err != nil {
					return err
				}
				if err := c.Do(ctx, "POST", "/npm/-/clowdr/token/rotate", npm.RotateRequest{Now: now}, &tr); err != nil {
					return err
				}
				reg = l.Registry
			} else {
				t, err := a.target(false)
				if err != nil {
					return err
				}
				if err := needNPMServer(ctx, t.client); err != nil {
					return err
				}
				if err := t.client.Do(ctx, "POST", "/v1/npm/tokens/"+url.PathEscape(args[0])+"/rotate", npm.RotateRequest{Now: now}, &tr); err != nil {
					return err
				}
				reg = registryURL(t.client.Server)
			}
			l := a.cfg.npmLogin(reg)
			if l == nil || l.Name != tr.Info.Name {
				fmt.Fprintf(a.out, "%s\n", tr.Token)
				fmt.Fprintf(a.err, "✓ npm token %s rotated, expires %s: give the new token (shown once) to its owner\n", tr.Info.Name, tr.Info.Expires.Format("2006-01-02"))
				return nil
			}
			files, err := writeHomeTokens(reg, tr.Token)
			if err != nil {
				return err
			}
			l.Token, l.Expires = tr.Token, tr.Info.Expires
			if err := a.cfg.Save(); err != nil {
				return err
			}
			old := "works 24 h more"
			if now {
				old = "stopped"
			}
			fmt.Fprintf(a.err, "✓ npm token %s rotated (the old one %s), expires %s; wrote %s\n", l.Name, old, l.Expires.Format("2006-01-02"), strings.Join(files, ", "))
			return nil
		},
	}
	rotate.Flags().BoolVar(&now, "now", false, "stop the old token at once instead of after 24 h")
	cmd.AddCommand(ls, revoke, rotate)
	return cmd
}
