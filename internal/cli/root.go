// Package cli implements clowctl.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/config"
)

type app struct {
	env         string
	project     string
	contextName string
	output      string
	version     string

	out io.Writer
	err io.Writer

	deploy  *config.Deploy // nil outside a project directory
	loadErr error
	cfg     *UserConfig
	// allowOutdated is deploy --allow-outdated: recorded in each release.
	allowOutdated bool
}

// target is where a command goes.
type target struct {
	client  *api.Client
	project string
}

func NewRoot(version string) *cobra.Command {
	return newRoot(&app{version: version, out: os.Stdout, err: os.Stderr})
}

func newRoot(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:           "clowctl",
		Short:         "Deploy locally built images to a clowdr server",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if cmd.Annotations[quietAnnotation] == "" {
				a.warnNPMExpiry(time.Now())
			}
			return nil
		},
	}
	f := root.PersistentFlags()
	f.StringVarP(&a.env, "env", "e", "", "environment from deploy.yml (default: dev, or the only env)")
	f.StringVarP(&a.project, "project", "p", "", "project (default: from deploy.yml)")
	f.StringVar(&a.contextName, "context", "", "context from ~/.clowdr/config")
	f.StringVarP(&a.output, "output", "o", "table", "output format: table|json")

	root.AddCommand(
		a.deployCmd(), a.applyCmd(), a.diffCmd(), a.rollbackCmd(), a.releasesCmd(),
		a.psCmd(), a.statusCmd(), a.routesCmd(), a.logsCmd(), a.accessCmd(), a.traceCmd(), a.metricsCmd(), a.analyticsCmd(), a.sdkCmd(), a.eventsCmd(), a.scaleCmd(), a.execCmd(), a.profileCmd(), a.topCmd(), a.volumeCmd(), a.addonCmd(), a.gcCmd(),
		a.restartCmd(), a.stopCmd(), a.startCmd(), a.rmCmd(), a.envCmd(),
		a.contextCmd(), a.projectCmd(), a.tokenCmd(), a.platformCmd(), a.versionCmd(), a.selfUpdateCmd(),
		a.npmCmd(), a.pluginsCmd(),
	)
	return root
}

// Execute runs clowctl; Ctrl+C cancels the current request.
func Execute(version string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := NewRoot(version).ExecuteContext(ctx); err != nil {
		var code exitError
		if errors.As(err, &code) {
			return int(code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (a *app) load() error {
	cfg, err := loadUserConfig()
	if err != nil {
		return err
	}
	a.cfg = cfg
	if p, err := config.FindDeploy("."); err == nil {
		a.deploy, a.loadErr = config.LoadDeploy(p)
	}
	if a.env == "" {
		a.env = "dev"
		if a.deploy != nil && len(a.deploy.Envs) == 1 {
			a.env = a.deploy.EnvNames()[0]
		}
	}
	return nil
}

// needDeploy returns deploy.yml or an error explaining why it is required.
func (a *app) needDeploy() (*config.Deploy, error) {
	if a.loadErr != nil {
		return nil, a.loadErr
	}
	if a.deploy == nil {
		return nil, errors.New("deploy.yml not found: run this command in a project directory")
	}
	return a.deploy, nil
}

// target resolves server, token and project.
//
// Server: --context, else envs.<env> in deploy.yml, else the current context.
// Token: CLOWDR_TOKEN, else .clowdr/token.<env> next to deploy.yml, else the context token.
// An explicit --context brings its own token: the project token file is skipped,
// so `--context admin` inside a project directory still talks as admin.
func (a *app) target(needProject bool) (*target, error) {
	var ctx *Context
	var server string
	switch {
	case a.contextName != "":
		ctx = a.cfg.Contexts[a.contextName]
		if ctx == nil {
			return nil, fmt.Errorf("context %q not found (clowctl context ls)", a.contextName)
		}
	case a.deploy != nil && len(a.deploy.Envs) > 0:
		e, ok := a.deploy.Envs[a.env]
		if !ok {
			return nil, fmt.Errorf("env %q is not in deploy.yml (have: %s)", a.env, strings.Join(a.deploy.EnvNames(), ", "))
		}
		if e.Context != "" {
			ctx = a.cfg.Contexts[e.Context]
			if ctx == nil {
				return nil, fmt.Errorf("deploy.yml envs.%s: context %q not found (clowctl context add)", a.env, e.Context)
			}
		} else {
			server = e.Server
			ctx = a.cfg.byServer(server)
		}
	case a.cfg.Current != "":
		ctx = a.cfg.Contexts[a.cfg.Current]
	}
	if ctx != nil && server == "" {
		server = ctx.Server
	}
	if server == "" {
		return nil, errors.New("no server: run in a project directory with deploy.yml, or add a context (clowctl context add)")
	}
	// CI pins the server its CLOWDR_TOKEN belongs to: a deploy.yml from an untrusted branch
	// could otherwise point envs.<env>.server at a host that collects the token.
	if pin := os.Getenv("CLOWDR_SERVER"); pin != "" && strings.TrimRight(pin, "/") != strings.TrimRight(server, "/") {
		return nil, fmt.Errorf("server %s is not CLOWDR_SERVER (%s): refusing to send the token there", server, pin)
	}

	token := os.Getenv("CLOWDR_TOKEN")
	if token == "" {
		token = os.Getenv("CLOWDER_TOKEN")
	}
	if token == "" && a.deploy != nil && a.contextName == "" {
		if b, err := os.ReadFile(filepath.Join(a.deploy.Dir, ".clowdr", "token."+a.env)); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" && ctx != nil {
		token = ctx.Token
	}
	if token == "" {
		return nil, fmt.Errorf("no token for %s: set CLOWDR_TOKEN, put it in .clowdr/token.%s, or clowctl context add", server, a.env)
	}
	fp := ""
	if ctx != nil {
		fp = ctx.Fingerprint
	}
	t := &target{client: api.NewClient(server, token, fp), project: a.project}
	if t.project == "" && a.deploy != nil {
		t.project = a.deploy.Project
	}
	if needProject && t.project == "" {
		return nil, errors.New("no project: run in a directory with deploy.yml or pass -p")
	}
	return t, nil
}

func (a *app) json() bool { return a.output == "json" }

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) table(header ...string) *tabwriter.Writer {
	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(header, "\t"))
	return w
}

func (a *app) progress(ev api.Event) {
	fmt.Fprintf(a.err, "  %s\n", ev.Message)
}
