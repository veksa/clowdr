package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// releasesBase is where release assets live; CLOWDR_REPO points it at a fork.
func releasesBase() string {
	repo := os.Getenv("CLOWDR_REPO")
	if repo == "" {
		repo = "veksa/clowdr"
	}
	return "https://github.com/" + repo + "/releases"
}

// releaseKey verifies checksums.txt.sig of official releases: the release workflow signs
// checksums.txt with the matching private key, kept in the repository's CI secrets. A fork
// (CLOWDR_REPO) signs with its own key and sets CLOWDR_RELEASE_KEY to its public half.
const releaseKey = "trLPx8MgJq3bXtfnnh2STD05iZqqL8msJ8UFlUJBbDU="

func releasePublicKey() (ed25519.PublicKey, error) {
	k := os.Getenv("CLOWDR_RELEASE_KEY")
	if k == "" {
		k = releaseKey
	}
	b, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad release key %q (base64 of a 32-byte ed25519 public key)", k)
	}
	return b, nil
}

func (a *app) selfUpdateCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Replace clowctl with the latest release (or --version) from GitHub Releases",
		Long: `Downloads clowctl for this OS and architecture from GitHub Releases, checks its sha256
against the release's checksums.txt, whose ed25519 signature must match the key built into
clowctl, and replaces the running binary. The server is updated by
running install.sh again on it; it keeps the state and restarts clowdrd, nginx keeps serving.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if p, err := filepath.EvalSymlinks(exe); err == nil {
				exe = p
			}
			if strings.HasPrefix(filepath.Base(exe), "clowdrd") {
				// On the server clowctl is a symlink to the daemon binary.
				return fmt.Errorf("%s is the server binary: update it with install.sh (curl -fsSL %s/latest/download/install.sh | sh)", exe, releasesBase())
			}
			key, err := releasePublicKey()
			if err != nil {
				return err
			}
			u := updater{base: releasesBase(), client: &http.Client{Timeout: 5 * time.Minute}, goos: goruntime.GOOS, goarch: goruntime.GOARCH, key: key}
			v, err := u.update(cmd.Context(), exe, a.version, version)
			switch {
			case errors.Is(err, errUpToDate):
				fmt.Fprintf(a.out, "clowctl %s is the latest\n", a.version)
				return nil
			case err != nil:
				return err
			}
			fmt.Fprintf(a.out, "clowctl %s → %s\n", a.version, v)
			fmt.Fprintf(a.out, "server: curl -fsSL %s/download/%s/install.sh | sh\n", u.base, v)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "release tag, e.g. v0.3.0 (default: latest)")
	return cmd
}

var errUpToDate = errors.New("up to date")

type updater struct {
	base         string // https://github.com/<repo>/releases
	client       *http.Client
	goos, goarch string
	key          ed25519.PublicKey // checksums.txt must carry its signature
}

// update installs release `want` ("" = latest) over exe and returns the installed version.
func (u updater) update(ctx context.Context, exe, current, want string) (string, error) {
	// A binary renamed away by a previous update on Windows.
	os.Remove(exe + ".old")
	if want == "" {
		latest, err := u.latest(ctx)
		if err != nil {
			return "", err
		}
		want = latest
	}
	if strings.TrimPrefix(want, "v") == strings.TrimPrefix(current, "v") {
		return want, errUpToDate
	}
	dl := u.base + "/download/" + want + "/"
	sums, err := u.get(ctx, dl+"checksums.txt")
	if err != nil {
		return "", err
	}
	sig, err := u.get(ctx, dl+"checksums.txt.sig")
	if err != nil {
		return "", fmt.Errorf("release %s is not signed: %w", want, err)
	}
	if !ed25519.Verify(u.key, sums, sig) {
		return "", fmt.Errorf("release %s: checksums.txt signature does not match the release key", want)
	}
	name, sum := "", ""
	for _, n := range u.assetNames() {
		if s, ok := findChecksum(sums, n); ok {
			name, sum = n, s
			break
		}
	}
	if name == "" {
		return "", fmt.Errorf("release %s has no clowctl for %s/%s", want, u.goos, u.goarch)
	}
	bin, err := u.get(ctx, dl+name)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(bin)
	if hex.EncodeToString(h[:]) != sum {
		return "", fmt.Errorf("%s: sha256 does not match checksums.txt", name)
	}
	return want, replaceExecutable(exe, bin)
}

func (u updater) assetNames() []string {
	n := fmt.Sprintf("clowctl_%s_%s", u.goos, u.goarch)
	if u.goos == "windows" {
		return []string{n + ".exe", n}
	}
	return []string{n}
}

// latest resolves /releases/latest through its redirect to /releases/tag/<version>,
// which needs no GitHub API token and has no API rate limit.
func (u updater) latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u.base+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := *u.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || !strings.Contains(loc, "/releases/tag/") {
		return "", fmt.Errorf("latest release: unexpected response %s", resp.Status)
	}
	return path.Base(loc), nil
}

func (u updater) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// findChecksum looks name up in a sha256sum-style file ("<hex>  <name>").
func findChecksum(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// replaceExecutable writes bin next to exe and swaps it in. A running executable cannot be
// overwritten on Windows but can be renamed, so the old one moves to exe.old first.
func replaceExecutable(exe string, bin []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".clowctl-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if goruntime.GOOS == "windows" {
		if err := os.Rename(exe, exe+".old"); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), exe); err != nil {
			os.Rename(exe+".old", exe)
			return err
		}
		return nil
	}
	return os.Rename(tmp.Name(), exe)
}
