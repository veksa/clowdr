package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/npm"
)

// pluginsMinServer is the first clowdrd with the Claude Code plugin marketplace.
const pluginsMinServer = "v0.5.0"

var errPluginsTooOld = fmt.Errorf("server too old for plugins (needs clowdrd %s or newer: clowdr self-update on the server)", pluginsMinServer)

// quietAnnotation marks a command whose output another program reads: no expiry warnings.
const quietAnnotation = "clowdr/quiet"

// helperMax is Claude Code's limit on a headersHelper command.
const helperMax = 500

// Replaced in tests.
var (
	executable = os.Executable
	argv0      = func() string { return os.Args[0] }
	lookPath   = exec.LookPath
	tempDir    = os.TempDir
	helperOS   = runtime.GOOS
)

// helperPath is the path of this clowctl for Claude Code to run: the path it was started by (as
// typed, or found on PATH) when that is this very file, so a link such as Homebrew's bin/clowctl
// survives an upgrade; else the file itself. It refuses a name that would start the server and
// a temporary build, and warns about a path an upgrade removes.
func helperPath() (path, warn string, err error) {
	exe, err := executable()
	if err != nil {
		return "", "", fmt.Errorf("cannot tell where clowctl is: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", "", err
	}
	path = exe
	if a := argv0(); a != "" {
		cand, err := a, error(nil)
		switch {
		case filepath.IsAbs(a):
		case strings.ContainsAny(a, `/\`):
			cand, err = filepath.Abs(a)
		default:
			if cand, err = lookPath(a); err == nil {
				cand, err = filepath.Abs(cand)
			}
		}
		if err == nil && sameFile(cand, exe) {
			path = filepath.Clean(cand)
		}
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if strings.HasPrefix(base, "clowdrd") {
		return "", "", fmt.Errorf("%s: the helper would run under a name that starts the server (clowdrd); run setup as clowctl, through a link or copy named clowctl", path)
	}
	slash := filepath.ToSlash(path)
	if strings.Contains(slash, "/go-build") || within(path, tempDir()) {
		return "", "", fmt.Errorf("%s is a temporary build, gone after it exits; install clowctl and run setup with the installed one", path)
	}
	if strings.Contains(slash, "/Cellar/") {
		warn = path + " is inside a Homebrew version folder, which an upgrade removes: run setup again after brew upgrade, or run it as the linked clowctl (`$(brew --prefix)/bin/clowctl plugins setup`)"
	}
	return path, warn, nil
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// within reports whether path is dir or inside it, comparing resolved paths where they exist.
func within(path, dir string) bool {
	if dir == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *app) pluginsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "plugins", Short: "Claude Code plugin marketplace: set it up in Claude Code, list what it offers"}
	cmd.AddCommand(a.pluginsSetupCmd(), a.pluginsHeadersCmd(), a.pluginsLsCmd())
	return cmd
}

// marketplaceURL is the server's marketplace.json for a registry URL.
func marketplaceURL(reg string) string { return registryServer(reg) + "/plugins/marketplace.json" }

// pluginsLogin is the npm login for the plugin commands. A login that records its token's scopes
// must have plugins:read; an older login, which did not record them, is left to the server.
func (a *app) pluginsLogin() (*NPMLogin, error) {
	l, err := a.npmLoginFor()
	if err != nil {
		return nil, fmt.Errorf("%w (with --scope %s)", err, auth.ScopePlugins)
	}
	if len(l.TokenScopes) > 0 && !slices.Contains(l.TokenScopes, auth.ScopePlugins) {
		return nil, fmt.Errorf("the npm token %s for %s has no %s (scopes %s), and a token's scopes cannot change: clowctl npm token revoke %s --context <admin context>, then clowctl npm login --context <admin context> --scope %s --scope <your package scopes>",
			l.Name, l.Registry, auth.ScopePlugins, strings.Join(l.TokenScopes, " "), l.Name, auth.ScopePlugins)
	}
	return l, nil
}

// fetchMarketplace reads the server's marketplace.json with the login's token.
func (a *app) fetchMarketplace(ctx context.Context, l *NPMLogin) (*npm.Marketplace, error) {
	c, err := a.npmTokenClient(ctx, l)
	if err != nil {
		return nil, err
	}
	var m npm.Marketplace
	if err := c.Do(ctx, "GET", "/plugins/marketplace.json", nil, &m); err != nil {
		var se *api.StatusError
		if errors.As(err, &se) && se.Status == 404 {
			// Go's mux on a server without /plugins/ answers in plain text; the marketplace
			// itself answers a token without plugins:read with the registry's JSON 404.
			if se.Message == "404 page not found" {
				return nil, errPluginsTooOld
			}
			return nil, fmt.Errorf("the server refuses its marketplace to the npm token %s: the token has no %s; revoke it and log in again with --scope %s",
				l.Name, auth.ScopePlugins, auth.ScopePlugins)
		}
		return nil, err
	}
	if m.Name == "" {
		return nil, errors.New("the server's marketplace.json has no name")
	}
	return &m, nil
}

func (a *app) pluginsSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Add this server's plugin marketplace to Claude Code (~/.claude/settings.json)",
		Long: `Adds the server's marketplace to extraKnownMarketplaces in Claude Code's user settings
(~/.claude/settings.json, or $CLAUDE_CONFIG_DIR/settings.json), under the marketplace's own name:

  "<name>": {"source": {"source": "url", "url": "https://deploy.example.com/plugins/marketplace.json",
             "headersHelper": "/usr/local/bin/clowctl plugins headers --registry https://deploy.example.com/npm/"},
             "autoUpdate": true}

Claude Code runs the helper for every fetch of the marketplace and of its plugin zips; it prints
the npm token of this machine from ~/.clowdr/config, so a rotated token needs no new setup.
The token needs the plugins:read scope (clowctl npm login --scope plugins:read --scope @acme).
Every other key of the file is kept; the file is written atomically with its mode, never through
a symlink into a git work tree. Running it again changes nothing when nothing changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.pluginsLogin()
			if err != nil {
				return err
			}
			if err := checkRegistryTransport(l.Registry); err != nil {
				return err
			}
			u, _ := url.Parse(l.Registry)
			if u.Scheme != "https" {
				fmt.Fprintf(a.err, "! %s is not https: Claude Code sends the token only over https, so only a local test works this way\n", l.Registry)
			}
			if a.registryPinned(l) {
				return fmt.Errorf("%s has a self-signed certificate (pinned in ~/.clowdr/config); Claude Code checks certificates, so plugins need the server on a domain (domain in platform.yml)", registryServer(l.Registry))
			}
			m, err := a.fetchMarketplace(cmd.Context(), l)
			if err != nil {
				return err
			}
			exe, warn, err := helperPath()
			if err != nil {
				return err
			}
			if warn != "" {
				fmt.Fprintf(a.err, "! %s\n", warn)
			}
			helper, err := headersHelperCommand(exe, l.Registry, helperOS)
			if err != nil {
				return err
			}
			path, err := claudeSettingsPath()
			if err != nil {
				return err
			}
			changed, mm, err := setMarketplace(path, m.Name, marketplaceURL(l.Registry), helper)
			if err != nil {
				return err
			}
			if mm.replaced != "" {
				fmt.Fprintf(a.err, "! the entry %s in %s was %s; it is replaced by this server's marketplace\n", m.Name, path, mm.replaced)
			}
			if changed {
				fmt.Fprintf(a.err, "✓ marketplace %s added to %s\n", m.Name, path)
			} else {
				fmt.Fprintf(a.err, "✓ marketplace %s is already set up in %s\n", m.Name, path)
			}
			for _, o := range mm.others {
				fmt.Fprintf(a.err, "! %s also lists this server as marketplace %q: remove that entry (claude plugin marketplace remove %s)\n", path, o, o)
			}
			if len(m.Plugins) == 0 {
				fmt.Fprintf(a.err, "  the token %s may read no plugin yet\n", l.Name)
			}
			plugin := "<plugin>"
			if len(m.Plugins) > 0 {
				plugin = m.Plugins[0].Name
			}
			fmt.Fprintf(a.err, "  next: start claude once, which adds the marketplace (in a running session: /plugin marketplace update %s)\n", m.Name)
			fmt.Fprintf(a.err, "        claude plugin install %s@%s\n", plugin, m.Name)
			if mm.manual {
				fmt.Fprintf(a.err, "  autoUpdate is false for %s in %s and stays so: new versions come only with\n", m.Name, path)
			} else {
				fmt.Fprintf(a.err, "  new versions install by themselves (autoUpdate); by hand:\n")
			}
			fmt.Fprintf(a.err, "        claude plugin marketplace update %s, then claude plugin update %s@%s\n", m.Name, plugin, m.Name)
			fmt.Fprintf(a.err, "  (claude plugin marketplace add with the URL is refused for a marketplace set up in settings)\n")
			for _, p := range m.Plugins {
				fmt.Fprintf(a.err, "  %s@%s %s\n", p.Name, m.Name, p.Version)
			}
			return nil
		},
	}
}

// registryPinned reports whether the login's server is reached through a pinned self-signed
// certificate.
func (a *app) registryPinned(l *NPMLogin) bool {
	srv := registryServer(l.Registry)
	if c := a.cfg.Contexts[l.Context]; c != nil && strings.TrimRight(c.Server, "/") == srv {
		return c.Fingerprint != ""
	}
	if c := a.cfg.byServer(srv); c != nil {
		return c.Fingerprint != ""
	}
	return false
}

func (a *app) pluginsHeadersCmd() *cobra.Command {
	var registry string
	cmd := &cobra.Command{
		Use:   "headers",
		Short: "Print the HTTP headers for this server's marketplace (Claude Code's headersHelper)",
		Long: `Prints {"Authorization":"Bearer <npm token>"} with the npm token ~/.clowdr/config keeps for the
registry, and nothing else on stdout. It makes no request. Claude Code runs it, as set up by
clowctl plugins setup, before every fetch of the marketplace and its plugin zips.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{quietAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := normRegistry(registry)
			if reg == "" {
				return errors.New("--registry is required, e.g. --registry https://deploy.example.com/npm/")
			}
			l := a.cfg.npmLogin(reg)
			if l == nil || l.Token == "" {
				return fmt.Errorf("no npm token for %s in ~/.clowdr/config: clowctl npm login --context <admin context> --scope %s --scope <your package scopes>", reg, auth.ScopePlugins)
			}
			b, err := json.Marshal(map[string]string{"Authorization": "Bearer " + l.Token})
			if err != nil {
				return err
			}
			_, err = a.out.Write(append(b, '\n'))
			return err
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "the npm registry URL the token is for (https://deploy.example.com/npm/)")
	return cmd
}

func (a *app) pluginsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the plugins this server's marketplace offers to this machine's npm token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.pluginsLogin()
			if err != nil {
				return err
			}
			m, err := a.fetchMarketplace(cmd.Context(), l)
			if err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(m)
			}
			fmt.Fprintf(a.err, "marketplace %s, %d plugin(s) for token %s\n", m.Name, len(m.Plugins), l.Name)
			w := a.table("PLUGIN", "VERSION", "DESCRIPTION")
			for _, p := range m.Plugins {
				fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name+"@"+m.Name, p.Version, p.Description)
			}
			return w.Flush()
		},
	}
}

// claudeSettingsPath is Claude Code's user settings file.
func claudeSettingsPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json"), nil
	}
	return homeFile(filepath.Join(".claude", "settings.json"))
}

var (
	shSafe   = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	urlChars = regexp.MustCompile(`^[A-Za-z0-9:/._\[\]-]+$`)
)

// headersHelperCommand is the command Claude Code runs for the headers: the absolute path of
// this clowctl, quoted for the shell that runs it (sh, or cmd.exe on Windows), then the
// arguments. On Windows the path is always in double quotes, with forward slashes, and nothing
// else is quoted: two quote characters, which cmd.exe keeps as they are, and inside them only
// '%' and '!' (variable expansion) and '"' mean anything, so those are refused. Claude Code takes
// at most 500 printable ASCII characters.
func headersHelperCommand(exe, reg, goos string) (string, error) {
	if !urlChars.MatchString(reg) {
		return "", fmt.Errorf("registry %q: unexpected characters for a command line", reg)
	}
	var cmd string
	if goos == "windows" {
		if strings.ContainsAny(exe, `"%!`) {
			return "", fmt.Errorf("%s: cmd.exe reads %%, ! and \" in a path as its own; install clowctl under a path without them", exe)
		}
		cmd = `"` + strings.ReplaceAll(exe, `\`, "/") + `" plugins headers --registry ` + reg
	} else {
		cmd = shQuote(exe) + " plugins headers --registry " + shQuote(reg)
	}
	for _, r := range cmd {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("the path of clowctl (%s) has characters Claude Code does not take in a command; install clowctl under a plain ASCII path", exe)
		}
	}
	if len(cmd) > helperMax {
		return "", fmt.Errorf("the headers command is %d characters, Claude Code takes at most %d; install clowctl under a shorter path", len(cmd), helperMax)
	}
	return cmd, nil
}

func shQuote(s string) string {
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// urlSource is the marketplace source Claude Code fetches by URL.
type urlSource struct {
	Source        string `json:"source"`
	URL           string `json:"url"`
	HeadersHelper string `json:"headersHelper"`
}

// marketMerge is what setMarketplace found in the file besides what it set.
type marketMerge struct {
	others   []string // other entries with this marketplace's URL
	replaced string   // what the entry of this name was, when it was another marketplace
	manual   bool     // the entry says "autoUpdate": false, which is kept
}

// setMarketplace writes extraKnownMarketplaces.<name> into a Claude Code settings file: source
// is set, and autoUpdate unless it is false already; every other key (of the file and of the
// entry) is kept in its place. It returns whether the file changed.
func setMarketplace(path, name, marketURL, helper string) (bool, marketMerge, error) {
	if err := checkLinkedFile(path, "a settings file", "replace the link with a regular file, or set CLAUDE_CONFIG_DIR to a folder that is not shared"); err != nil {
		return false, marketMerge{}, err
	}
	target, err := resolveTarget(path)
	if err != nil {
		return false, marketMerge{}, err
	}
	mode := os.FileMode(0o644)
	old, err := os.ReadFile(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, marketMerge{}, err
	default:
		if fi, err := os.Stat(target); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	out, mm, err := mergeMarketplace(old, name, marketURL, helper)
	if err != nil {
		return false, marketMerge{}, fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(out, old) {
		return false, mm, nil
	}
	if err := writeFileAtomic(path, out, mode); err != nil {
		return false, marketMerge{}, err
	}
	return true, mm, nil
}

// mergeMarketplace is setMarketplace on the file's bytes. The result is indented with two spaces.
func mergeMarketplace(old []byte, name, marketURL, helper string) ([]byte, marketMerge, error) {
	var mm marketMerge
	root, err := parseObject(old)
	if err != nil {
		return nil, mm, err
	}
	markets := &object{}
	if raw, ok := root.get("extraKnownMarketplaces"); ok {
		if markets, err = parseObject(raw); err != nil {
			return nil, mm, fmt.Errorf("extraKnownMarketplaces: %w", err)
		}
	}
	for _, k := range markets.keys {
		if k == name {
			continue
		}
		var e struct {
			Source struct {
				URL string `json:"url"`
			} `json:"source"`
		}
		if json.Unmarshal(markets.vals[k], &e) == nil && e.Source.URL == marketURL {
			mm.others = append(mm.others, k)
		}
	}
	entry := &object{}
	if raw, ok := markets.get(name); ok {
		e, err := parseObject(raw)
		if err != nil {
			mm.replaced = "not an object (" + clip(string(raw)) + ")"
		} else {
			entry = e
			var cur struct {
				Source struct {
					Source string `json:"source"`
					URL    string `json:"url"`
					Repo   string `json:"repo"`
				} `json:"source"`
			}
			json.Unmarshal(raw, &cur)
			switch src := cur.Source; {
			case src.URL == marketURL:
			case src.URL != "":
				mm.replaced = "the marketplace at " + clip(src.URL)
			case src.Repo != "":
				mm.replaced = "the " + clip(src.Source) + " marketplace " + clip(src.Repo)
			default:
				mm.replaced = "another marketplace (" + clip(string(raw)) + ")"
			}
			if v, ok := e.get("autoUpdate"); ok && string(bytes.TrimSpace(v)) == "false" {
				mm.manual = true
			}
		}
	}
	src, err := encodeJSON(urlSource{Source: "url", URL: marketURL, HeadersHelper: helper})
	if err != nil {
		return nil, mm, err
	}
	entry.set("source", src)
	if !mm.manual {
		entry.set("autoUpdate", json.RawMessage("true"))
	}
	markets.set(name, entry.encode())
	root.set("extraKnownMarketplaces", markets.encode())
	var buf bytes.Buffer
	if err := json.Indent(&buf, root.encode(), "", "  "); err != nil {
		return nil, mm, err
	}
	buf.WriteByte('\n')
	// Unchanged content keeps the file as it was, whatever its formatting.
	if len(bytes.TrimSpace(old)) > 0 {
		var a, b bytes.Buffer
		if json.Compact(&a, old) == nil && json.Compact(&b, buf.Bytes()) == nil && bytes.Equal(a.Bytes(), b.Bytes()) {
			return old, mm, nil
		}
	}
	return buf.Bytes(), mm, nil
}

// clip shortens s for a message.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func encodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// object is a JSON object that keeps its keys in order.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

// parseObject reads a JSON object; empty input is an empty object. A key given twice keeps its
// first place and its last value, as a JSON reader would see it.
func parseObject(b []byte) (*object, error) {
	o := &object{vals: map[string]json.RawMessage{}}
	if len(bytes.TrimSpace(b)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not valid JSON: %w", err)
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("not valid JSON: %w", err)
		}
		o.set(k, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("not valid JSON: data after the object")
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := encodeJSON(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}
