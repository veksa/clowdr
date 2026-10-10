package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/sdk"
)

// vendorDir is where `clowctl sdk install` puts the tarball, relative to deploy.yml. It is
// committed: builds then need neither the server nor the network to install it.
const vendorDir = "vendor/clowdr"

func (a *app) sdkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sdk",
		Short: "Client packages served by the server (clowdr-node: traces, docs/tracing.md)",
	}
	install := func(use, short string) *cobra.Command {
		var version string
		c := &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.sdkInstall(cmd, version)
			},
		}
		c.Flags().StringVar(&version, "version", "", "this version instead of the server's newest")
		return c
	}
	cmd.AddCommand(
		install("install", "Download clowdr-node into vendor/clowdr and add it to every Node service of deploy.yml"),
		install("update", "Move to the server's newest clowdr-node"),
		&cobra.Command{
			Use:   "ls",
			Short: "Versions the server offers",
			RunE: func(cmd *cobra.Command, args []string) error {
				idx, err := a.sdkIndex(cmd)
				if err != nil {
					return err
				}
				if a.json() {
					return a.printJSON(idx)
				}
				for _, p := range idx.Packages {
					fmt.Fprintf(a.out, "%s %s\n", p.Name, p.Version)
				}
				return nil
			},
		},
	)
	return cmd
}

func (a *app) sdkIndex(cmd *cobra.Command) (sdk.Index, error) {
	var idx sdk.Index
	t, err := a.target(false)
	if err != nil {
		return idx, err
	}
	body, err := t.client.Raw(cmd.Context(), "GET", "/sdk/node/")
	if err != nil {
		return idx, err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(&idx); err != nil {
		return idx, fmt.Errorf("server sent no package index (clowdrd too old for clowctl sdk?): %w", err)
	}
	return idx, nil
}

func (a *app) sdkInstall(cmd *cobra.Command, version string) error {
	d, err := a.needDeploy()
	if err != nil {
		return err
	}
	idx, err := a.sdkIndex(cmd)
	if err != nil {
		return err
	}
	if version == "" {
		version = idx.Latest
	}
	var file string
	for _, p := range idx.Packages {
		if p.Version == version {
			file = p.File
		}
	}
	if file == "" {
		return fmt.Errorf("the server has no clowdr-node %q", version)
	}
	t, err := a.target(false)
	if err != nil {
		return err
	}
	body, err := t.client.Raw(cmd.Context(), "GET", "/sdk/node/"+file)
	if err != nil {
		return err
	}
	defer body.Close()
	tgz, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	dir := filepath.Join(d.Dir, vendorDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(dir, "clowdr-node-*.tgz"))
	dest := filepath.Join(dir, file)
	if err := os.WriteFile(dest, tgz, 0o644); err != nil {
		return err
	}
	for _, o := range old {
		if o != dest {
			os.Remove(o)
		}
	}
	fmt.Fprintf(a.out, "%s/%s\n", vendorDir, file)

	// Every service with its own package.json gets the dependency.
	names := make([]string, 0, len(d.Services))
	for n := range d.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, n := range names {
		p := d.Services[n].Path
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		pj := filepath.Join(d.Dir, p, "package.json")
		src, err := os.ReadFile(pj)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(filepath.Join(d.Dir, p), dest)
		if err != nil {
			return err
		}
		out, err := setDependency(src, "clowdr-node", "file:"+filepath.ToSlash(rel))
		if err != nil {
			fmt.Fprintf(a.out, "  %s: %v — add \"clowdr-node\": \"file:%s\" by hand\n", n, err, filepath.ToSlash(rel))
			continue
		}
		if err := os.WriteFile(pj, out, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "  %s: %s/package.json → clowdr-node %s (run the package manager's install there)\n", n, p, version)
	}
	fmt.Fprintln(a.out, "Wiring and reading traces: node_modules/clowdr-node/README.md, or clowctl trace --help")
	return nil
}

var depValue = regexp.MustCompile(`("clowdr-node"\s*:\s*)"[^"]*"`)

// setDependency sets dependencies[name] in package.json text, keeping its formatting.
func setDependency(src []byte, name, value string) ([]byte, error) {
	s := string(src)
	quoted, _ := json.Marshal(value)
	if name == "clowdr-node" && depValue.MatchString(s) {
		s = depValue.ReplaceAllString(s, "${1}"+strings.ReplaceAll(string(quoted), "$", "$$"))
	} else {
		loc := regexp.MustCompile(`"dependencies"\s*:\s*\{`).FindStringIndex(s)
		if loc == nil {
			return nil, errors.New(`package.json has no "dependencies"`)
		}
		rest := s[loc[1]:]
		indent := "    "
		if m := regexp.MustCompile(`^\s*\n([ \t]+)"`).FindStringSubmatch(rest); m != nil {
			indent = m[1]
		}
		entry := "\n" + indent + `"` + name + `": ` + string(quoted)
		if strings.HasPrefix(strings.TrimSpace(rest), "}") {
			s = s[:loc[1]] + entry + "\n" + strings.Repeat(" ", max(0, len(indent)-2)) + strings.TrimLeft(rest, " \t\r\n")
		} else {
			s = s[:loc[1]] + entry + "," + rest
		}
	}
	if !json.Valid([]byte(s)) {
		return nil, errors.New("could not edit package.json")
	}
	return []byte(s), nil
}
