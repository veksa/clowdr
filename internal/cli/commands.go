package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/monitor"
	"github.com/veksa/clowdr/internal/store"
)

func (a *app) deployCmd() *cobra.Command {
	var noBuild, noGC bool
	var image, tag, keepCache string
	cmd := &cobra.Command{
		Use:   "deploy [service...]",
		Short: "Build locally, push new layers, switch traffic after health checks",
		Long: `Build each service locally (build command + docker build), push only the new layers to the
server's registry and deploy: the new version starts next to the old one, traffic switches
when it is healthy, the old one is drained. Without arguments deploys every service in order.

After a service is live its older local images are removed, keeping the deployed one and the
one before it (the server keeps the same two), and the build cache is trimmed to --keep-cache.

A project that uses packages from a clowdr npm registry is checked first (clowctl npm check):
a deprecated or removed locked version, or a private scope the project does not map to its
registry, stops the deploy. --allow-outdated deploys anyway and is recorded in the release.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.needDeploy()
			if err != nil {
				return err
			}
			t, err := a.target(true)
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				names = d.ServiceNames()
			}
			if image != "" && len(names) != 1 {
				return errors.New("--image needs exactly one service")
			}
			if a.allowOutdated {
				// Recorded in each release, on servers that know the field.
				a.allowOutdated = a.confirmAllowOutdated(cmd.Context(), t.client)
			} else if err := a.npmCheckDeploy(cmd.Context(), d); err != nil {
				return err
			}
			if image == "" && slices.ContainsFunc(names, func(n string) bool { return usesDocker(d.Services[n]) }) {
				if err := requireLocalDocker(cmd.Context()); err != nil {
					return err
				}
				if !noBuild {
					if err := requireBuildx(cmd.Context()); err != nil {
						return err
					}
				}
			}
			built := false
			for _, name := range names {
				ref, err := a.deployOne(cmd.Context(), d, t, name, noBuild, image, tag)
				if err != nil {
					return err
				}
				if ref == "" || noGC {
					continue
				}
				built = true
				// The tag is always there, so the last colon is its, not a registry port's.
				refs, err := pruneLocalImages(cmd.Context(), ref[:strings.LastIndex(ref, ":")], ref)
				if err != nil {
					fmt.Fprintf(a.err, "! %s: local images not cleaned: %v\n", name, err)
				} else if len(refs) > 0 {
					fmt.Fprintf(a.err, "→ %s: removed %d old local images\n", name, len(refs))
				}
			}
			// Once per run, not per service: with the cache under the limit it is a no-op.
			if built && keepCache != "" {
				if out, err := exec.CommandContext(cmd.Context(), "docker", "builder", "prune", "-f", "--keep-storage", keepCache).CombinedOutput(); err != nil {
					fmt.Fprintf(a.err, "! build cache not trimmed: %v %s\n", err, strings.TrimSpace(string(out)))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBuild, "no-build", false, "push an image that was already built locally")
	cmd.Flags().StringVar(&image, "image", "", "deploy this image reference (pulled by the server)")
	cmd.Flags().StringVar(&tag, "tag", "", "image tag (default: git commit)")
	cmd.Flags().BoolVar(&noGC, "no-gc", false, "keep older local images and the build cache as they are")
	cmd.Flags().StringVar(&keepCache, "keep-cache", "20GB", "build cache to keep after deploying (empty: do not touch)")
	cmd.Flags().BoolVar(&a.allowOutdated, "allow-outdated", false, "skip the check of locked packages from clowdr registries (clowctl npm check); recorded in the release")
	return cmd
}

// deployOne returns the local image it built or pushed, empty when the server pulls one itself.
func (a *app) deployOne(ctx context.Context, d *config.Deploy, t *target, name string, noBuild bool, image, tag string) (string, error) {
	sp, err := d.Spec(name, a.env)
	if err != nil {
		return "", err
	}
	var ref string
	s := d.Services[name]
	dir := filepath.Join(d.Dir, filepath.FromSlash(s.Path))
	req := api.DeployRequest{Spec: sp, Author: gitAuthor(), AllowOutdated: a.allowOutdated}
	start := time.Now()
	switch {
	case image != "":
		req.Image, req.Version = image, image
	case !usesDocker(s):
		req.Image, req.Version = s.Image, s.Image
	default:
		if tag == "" {
			tag = versionTag(dir)
		}
		ref = fmt.Sprintf("%s/%s/%s:%s", t.client.Host(), t.project, name, tag)
		if noBuild {
			if err := exec.CommandContext(ctx, "docker", "image", "inspect", ref).Run(); err != nil {
				return "", fmt.Errorf("--no-build: image %s is not built locally", ref)
			}
		} else if err := a.buildImage(ctx, d, name, ref); err != nil {
			return "", err
		}
		fmt.Fprintf(a.err, "→ %s: push %s\n", name, ref)
		if err := pushImage(ctx, ref, t.client.Host(), t.project, t.client.Token); err != nil {
			return "", fmt.Errorf("push %s: %w", name, err)
		}
		req.Tag, req.Version = tag, tag
	}
	fmt.Fprintf(a.err, "→ %s: deploy to %s (%s)\n", name, t.client.Server, a.env)
	ev, err := t.client.Stream(ctx, "POST", api.Path("projects", t.project, "services", name, "deploy"), req, a.progress)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	fmt.Fprintf(a.err, "✓ %s r%d live in %s\n", name, ev.Release.ID, time.Since(start).Round(100*time.Millisecond))
	return ref, nil
}

func (a *app) applyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply [service...]",
		Short: "Apply deploy.yml changes without a new image (routes reload nginx; limits/env redeploy)",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.needDeploy()
			if err != nil {
				return err
			}
			t, err := a.target(true)
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				names = d.ServiceNames()
			}
			req := api.ApplyRequest{Author: gitAuthor()}
			for _, n := range names {
				sp, err := d.Spec(n, a.env)
				if err != nil {
					return err
				}
				req.Services = append(req.Services, sp)
			}
			if _, err = t.client.Stream(cmd.Context(), "POST", api.Path("projects", t.project, "apply"), req, a.progress); err != nil {
				return err
			}
			return a.applyAnalytics(cmd.Context(), t, d)
		},
	}
}

func (a *app) rollbackCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "rollback <service>",
		Short: "Go back to the previous release (the image is still on the server)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			req := api.RollbackRequest{Author: gitAuthor()}
			if to != "" {
				id, err := strconv.ParseUint(strings.TrimPrefix(to, "r"), 10, 64)
				if err != nil {
					return fmt.Errorf("--to: expected a release id like r12")
				}
				req.To = id
			}
			ev, err := t.client.Stream(cmd.Context(), "POST", api.Path("projects", t.project, "services", args[0], "rollback"), req, a.progress)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.err, "✓ %s rolled back, now r%d (%s)\n", args[0], ev.Release.ID, ev.Release.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "release id (default: previous)")
	return cmd
}

func (a *app) releasesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "releases <service>",
		Short: "Release history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var rs []store.Release
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services", args[0], "releases"), nil, &rs); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(rs)
			}
			w := a.table("RELEASE", "VERSION", "STATUS", "AUTHOR", "CREATED", "DURATION", "ERROR")
			for _, r := range rs {
				dur := ""
				if !r.Finished.IsZero() {
					dur = r.Finished.Sub(r.Created).Round(time.Second).String()
				}
				status := string(r.Status)
				if r.Rollback {
					status += " (rollback)"
				}
				if r.AllowOutdated {
					status += " (allow-outdated)"
				}
				fmt.Fprintf(w, "r%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Version, status, r.Author, ago(r.Created), dur, firstLine(r.Error))
			}
			return w.Flush()
		},
	}
}

func (a *app) psCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ps",
		Aliases: []string{"ls"},
		Short:   "Services of the project: replicas, state, release, memory",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var ss []api.ServiceStatus
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services")+"?stats=1", nil, &ss); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(ss)
			}
			w := a.table("SERVICE", "READY", "STATE", "RELEASE", "VERSION", "UPTIME", "CPU", "MEM / LIMIT", "RESTARTS")
			for _, s := range ss {
				ready, restarts := 0, 0
				var mem int64
				var cpu float64
				states := map[string]int{}
				var started time.Time
				for _, c := range s.Containers {
					states[c.State]++
					if c.State == "running" {
						ready++
					}
					restarts += c.Restarts
					mem += c.MemUsage
					cpu += c.CPU
					if started.IsZero() || c.Started.Before(started) {
						started = c.Started
					}
				}
				state := "running"
				switch {
				case s.Stopped:
					state = "stopped"
				case s.Release == nil:
					state = "not deployed"
				case ready < len(s.Containers):
					state = fmt.Sprint(states)
				}
				rel, ver := "-", "-"
				if s.Release != nil {
					rel, ver = fmt.Sprintf("r%d", s.Release.ID), s.Release.Version
				}
				uptime := "-"
				if !started.IsZero() && ready > 0 {
					uptime = time.Since(started).Round(time.Second).String()
				}
				limit := "-"
				if s.Memory > 0 {
					limit = config.FormatBytes(s.Memory * int64(max(len(s.Containers), 1)))
				}
				fmt.Fprintf(w, "%s\t%d/%d\t%s\t%s\t%s\t%s\t%.1f%%\t%s / %s\t%d\n", s.Name, ready, s.Replicas, state, rel, ver, uptime, cpu, human(mem), limit, restarts)
			}
			return w.Flush()
		},
	}
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <service>",
		Short: "Details of a service: containers, domains, routes, last release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var s api.ServiceStatus
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services", args[0]), nil, &s); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(s)
			}
			fmt.Fprintf(a.out, "Service:   %s/%s\n", t.project, s.Name)
			if s.Release != nil {
				r := s.Release
				fmt.Fprintf(a.out, "Release:   r%d %s by %s, %s\n", r.ID, r.Version, r.Author, ago(r.Created))
				fmt.Fprintf(a.out, "Image:     %s\n", r.Image)
			}
			fmt.Fprintf(a.out, "Replicas:  %d   Memory: %s   Port: %d   Health: %s\n", s.Replicas, config.FormatBytes(s.Memory), s.Spec.Port, orDash(s.Spec.Health.Path))
			fmt.Fprintf(a.out, "Domains:   %s\n", orDash(strings.Join(s.Domains, ", ")))
			for _, r := range s.Spec.Routes {
				target := s.Name
				if r.Service != "" {
					target = r.Service
				}
				fmt.Fprintf(a.out, "Route:     %s → %s\n", r.Path, target)
			}
			if s.Stopped {
				fmt.Fprintln(a.out, "State:     stopped")
			}
			fmt.Fprintln(a.out)
			w := a.table("CONTAINER", "STATE", "STARTED", "RESTARTS", "CPU", "MEM")
			for _, c := range s.Containers {
				state := c.State
				if c.OOM {
					state += " (OOM)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%.1f%%\t%s\n", c.Name, state, ago(c.Started), c.Restarts, c.CPU, human(c.MemUsage))
			}
			return w.Flush()
		},
	}
}

func (a *app) routesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "routes",
		Short: "The nginx config generated for the project",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var s string
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "routes"), nil, &s); err != nil {
				return err
			}
			fmt.Fprint(a.out, s)
			return nil
		},
	}
}

func (a *app) logsCmd() *cobra.Command {
	var follow, all bool
	var since, until, grep, release string
	var download, timestamps bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs [service]",
		Short: "Service logs (last 100 lines; -f to stream)",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var names []string
			switch {
			case all:
				var ss []api.ServiceStatus
				if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services"), nil, &ss); err != nil {
					return err
				}
				for _, s := range ss {
					names = append(names, s.Name)
				}
			case len(args) == 1:
				names = args
			default:
				return errors.New("name a service or pass --all")
			}
			q := url.Values{"tail": {strconv.Itoa(tail)}}
			if follow {
				q.Set("follow", "1")
			}
			if since != "" {
				q.Set("since", since)
			}
			if until != "" {
				q.Set("until", until)
			}
			if download && !cmd.Flags().Changed("tail") {
				q.Set("tail", "0")
			}
			if grep != "" {
				q.Set("grep", grep)
			}
			if release != "" {
				q.Set("release", release)
			}
			errc := make(chan error, len(names))
			var mu sync.Mutex // one stream per service, one writer
			for _, n := range names {
				go func() {
					path := api.Path("projects", t.project, "services", n, "logs") + "?" + q.Encode()
					errc <- t.client.Logs(cmd.Context(), path, func(l api.LogLine) {
						mu.Lock()
						defer mu.Unlock()
						prefix := replicaOf(l.Container)
						if all {
							prefix = n + prefix
						}
						switch {
						case download:
							fmt.Fprintf(a.out, "%s %s %s\n", l.Time, l.Container, l.Line)
						case timestamps:
							fmt.Fprintf(a.out, "%s [%s] %s\n", shortTime(l.Time), prefix, l.Line)
						default:
							fmt.Fprintf(a.out, "[%s] %s\n", prefix, l.Line)
						}
					})
				}()
			}
			var first error
			for range names {
				if err := <-errc; err != nil && first == nil && !errors.Is(err, context.Canceled) {
					first = err
				}
			}
			return first
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "stream new lines")
	f.BoolVar(&all, "all", false, "all services of the project")
	f.IntVar(&tail, "tail", 100, "lines from the end")
	f.StringVar(&since, "since", "", "only newer than, e.g. 2h, 7d or an RFC 3339 time")
	f.StringVar(&until, "until", "", "only older than, e.g. 1h")
	f.BoolVar(&download, "download", false, "plain output of the whole range (with --since), for saving to a file")
	f.BoolVarP(&timestamps, "timestamps", "t", false, "show times")
	f.StringVar(&grep, "grep", "", "regular expression, filtered on the server (ERROR|timeout)")
	f.StringVar(&release, "release", "", "logs of a release (e.g. a failed one), like r12")
	return cmd
}

// replicaOf turns shop-server-r12-2 into "#2".
func replicaOf(container string) string {
	if i := strings.LastIndexByte(container, '-'); i >= 0 {
		return "#" + container[i+1:]
	}
	return container
}

func (a *app) restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <service>",
		Short: "Fresh containers from the same image, switched by health",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			_, err = t.client.Stream(cmd.Context(), "POST", api.Path("projects", t.project, "services", args[0], "restart"), api.RestartRequest{Author: gitAuthor()}, a.progress)
			return err
		},
	}
}

func (a *app) stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <service>",
		Short: "Stop a service (keeps it, its routes answer 502)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			return t.client.Do(cmd.Context(), "POST", api.Path("projects", t.project, "services", args[0], "stop"), nil, nil)
		},
	}
}

func (a *app) startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <service>",
		Short: "Start a stopped service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			_, err = t.client.Stream(cmd.Context(), "POST", api.Path("projects", t.project, "services", args[0], "start"), nil, a.progress)
			return err
		},
	}
}

func (a *app) rmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <service>",
		Short: "Remove a service, its routes and images (volumes stay)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Remove %s/%s from %s?", t.project, args[0], t.client.Server)) {
				return errors.New("aborted")
			}
			return t.client.Do(cmd.Context(), "DELETE", api.Path("projects", t.project, "services", args[0]), nil, nil)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

func (a *app) envCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "env", Short: "Environment variables of a service (applied on the next deploy or restart)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "set <service> KEY=value...",
		Short: "Set variables",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			req := api.EnvRequest{Set: map[string]string{}}
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("%q: expected KEY=value", kv)
				}
				req.Set[k] = v
			}
			if err := t.client.Do(cmd.Context(), "PATCH", api.Path("projects", t.project, "services", args[0], "env"), req, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.err, "set; run `clowctl restart %s` to apply\n", args[0])
			return nil
		},
	}, &cobra.Command{
		Use:   "ls <service>",
		Short: "List variable names",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var keys []string
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services", args[0], "env"), nil, &keys); err != nil {
				return err
			}
			for _, k := range keys {
				fmt.Fprintln(a.out, k)
			}
			return nil
		},
	}, &cobra.Command{
		Use:   "rm <service> KEY...",
		Short: "Remove variables",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			return t.client.Do(cmd.Context(), "PATCH", api.Path("projects", t.project, "services", args[0], "env"), api.EnvRequest{Unset: args[1:]}, nil)
		},
	})
	return cmd
}

// Contexts

func (a *app) contextCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "context", Short: "Servers and tokens in ~/.clowdr/config"}
	var token, fingerprint string
	var insecure bool
	add := &cobra.Command{
		Use:   "add <name> <server-url>",
		Short: "Add a server (https://deploy.example.com or https://<ip>:7443)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, server := args[0], strings.TrimRight(args[1], "/")
			if token == "" {
				return errors.New("--token is required")
			}
			c := &Context{Server: server, Token: token, Fingerprint: fingerprint}
			if strings.HasPrefix(server, "https://") && fingerprint == "" && insecure {
				fp, err := fetchFingerprint(server)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.err, "pinned certificate %s\n", fp)
				c.Fingerprint = fp
			}
			client := api.NewClient(server, token, c.Fingerprint)
			var v api.Version
			if err := client.Do(cmd.Context(), "GET", "/v1/version", nil, &v); err != nil {
				return fmt.Errorf("cannot reach %s: %w", server, err)
			}
			if c.Fingerprint != "" {
				if err := trustForDocker(cmd.Context(), server, c.Fingerprint, a.err); err != nil {
					fmt.Fprintf(a.err, "warning: %v\n", err)
				}
			}
			a.cfg.Contexts[name] = c
			if a.cfg.Current == "" {
				a.cfg.Current = name
			}
			if err := a.cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(a.err, "context %s → %s (clowdrd %s, docker %s, as %s)\n", name, server, v.Version, v.Docker, v.Principal)
			return nil
		},
	}
	add.Flags().StringVar(&token, "token", "", "token (admin or project)")
	add.Flags().StringVar(&fingerprint, "fingerprint", "", "sha256 fingerprint of a self-signed certificate (printed by install.sh)")
	add.Flags().BoolVar(&insecure, "trust-on-first-use", false, "pin whatever certificate the server presents now")
	cmd.AddCommand(add, &cobra.Command{
		Use:   "ls",
		Short: "List contexts",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := a.table("CURRENT", "NAME", "SERVER")
			for name, c := range a.cfg.Contexts {
				cur := ""
				if name == a.cfg.Current {
					cur = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", cur, name, c.Server)
			}
			return w.Flush()
		},
	}, &cobra.Command{
		Use:   "use <name>",
		Short: "Set the default context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.cfg.Contexts[args[0]] == nil {
				return fmt.Errorf("context %q not found", args[0])
			}
			a.cfg.Current = args[0]
			return a.cfg.Save()
		},
	}, &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			delete(a.cfg.Contexts, args[0])
			if a.cfg.Current == args[0] {
				a.cfg.Current = ""
			}
			return a.cfg.Save()
		},
	})
	return cmd
}

func fetchFingerprint(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	conn, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return api.CertFingerprint(conn.ConnectionState().PeerCertificates[0]), nil
}

// trustForDocker installs the server's self-signed certificate for docker push:
// ~/.docker/certs.d (Docker Desktop, OrbStack), /etc/docker/certs.d (Linux engine) or that of
// a running colima VM. The certificate must match the pinned fingerprint.
func trustForDocker(ctx context.Context, server, fingerprint string, log io.Writer) error {
	u, _ := url.Parse(server)
	conn, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	cert := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	if !strings.EqualFold(api.CertFingerprint(cert), fingerprint) {
		return fmt.Errorf("%s presented a certificate that does not match the pinned fingerprint", u.Host)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	dir := certsDirName(u.Host)
	var dirs []string
	if home, err := homeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".docker", "certs.d", dir))
	}
	if goruntime.GOOS == "linux" {
		dirs = append(dirs, filepath.Join("/etc/docker/certs.d", dir))
	}
	var lastErr error
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			lastErr = err
			continue
		}
		if err := os.WriteFile(filepath.Join(d, "ca.crt"), pemBytes, 0o644); err != nil {
			lastErr = err
			continue
		}
		fmt.Fprintf(log, "docker trusts %s via %s\n", u.Host, filepath.Join(d, "ca.crt"))
	}
	// colima copies ~/.docker/certs.d into its VM only on start; until then its dockerd, which
	// reads /etc/docker/certs.d in the VM, fails the push with x509.
	if profile, ok := colimaProfile(dockerEndpoint(ctx)); ok {
		target := "/etc/docker/certs.d/" + dir
		cmd := exec.CommandContext(ctx, "colima", "ssh", "-p", profile, "--", "sudo", "sh", "-c", "mkdir -p '"+target+"' && cat > '"+target+"/ca.crt'")
		cmd.Stdin = bytes.NewReader(pemBytes)
		if out, err := cmd.CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("colima %s: %v %s; run: colima restart %s", profile, err, strings.TrimSpace(string(out)), profile)
		} else {
			fmt.Fprintf(log, "docker trusts %s via colima %s:%s/ca.crt\n", u.Host, profile, target)
		}
	}
	switch {
	case lastErr != nil && goruntime.GOOS == "linux":
		return fmt.Errorf("%v; run: sudo mkdir -p /etc/docker/certs.d/%s && sudo cp ~/.docker/certs.d/%s/ca.crt /etc/docker/certs.d/%s/", lastErr, dir, dir, dir)
	case lastErr != nil:
		return lastErr
	case goruntime.GOOS == "windows":
		fmt.Fprintf(log, "if docker push still fails with an x509 error, trust it in Windows: certutil -user -addstore Root %s\n", filepath.Join(dirs[0], "ca.crt"))
	}
	return nil
}

// colimaProfile reports the colima profile whose docker socket endpoint is:
// ~/.colima/<profile>/docker.sock, ~/.colima/docker.sock for the default one, or the same
// under $XDG_CONFIG_HOME/colima.
func colimaProfile(endpoint string) (string, bool) {
	p, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || path.Base(p) != "docker.sock" {
		return "", false
	}
	dir := path.Dir(p)
	switch parent := path.Base(path.Dir(dir)); {
	case path.Base(dir) == ".colima" || path.Base(dir) == "colima":
		return "default", true
	case parent == ".colima" || parent == "colima":
		return path.Base(dir), true
	}
	return "", false
}

// certsDirName is the certs.d directory docker looks up for a registry host: host:port, or
// hostport on Windows where ':' cannot be in a file name (moby's cleanPath).
func certsDirName(host string) string {
	if goruntime.GOOS == "windows" {
		return strings.ReplaceAll(host, ":", "")
	}
	return host
}

// Admin

func (a *app) projectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Projects on the server (admin)"}
	var domains, addons []string
	var memory string
	var nginxExtra, adopt bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create or update a project with its allowed domains and memory quota",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			mem, err := config.ParseBytes(memory)
			if err != nil {
				return err
			}
			req := api.ProjectRequest{Name: args[0], Domains: domains, Addons: addons, Memory: mem, NginxExtra: nginxExtra, AdoptVolumes: adopt}
			return t.client.Do(cmd.Context(), "POST", "/v1/projects", req, nil)
		},
	}
	create.Flags().StringSliceVar(&domains, "domains", nil, "allowed domains, e.g. example.org,*.example.org")
	create.Flags().StringSliceVar(&addons, "platform", nil, "addons the project may run, e.g. redis")
	create.Flags().StringVar(&memory, "memory", "", "memory quota for all services, e.g. 300m")
	create.Flags().BoolVar(&nginxExtra, "nginx-extra", false, "allow raw nginx snippets (trusted projects only)")
	create.Flags().BoolVar(&adopt, "adopt-volumes", false, "a new project takes over the volumes of a removed project of the same name")
	var yes, rmVolumes bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a project: services, addons, routes, tokens (volumes stay unless --volumes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			what := "all its services and addons"
			path := api.Path("projects", args[0])
			if rmVolumes {
				what += ", and DELETE ITS DATA (volumes)"
				path += "?volumes=1"
			}
			if !yes && !confirm(fmt.Sprintf("Remove project %s with %s from %s?", args[0], what, t.client.Server)) {
				return errors.New("aborted")
			}
			return t.client.Do(cmd.Context(), "DELETE", path, nil, nil)
		},
	}
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	rm.Flags().BoolVar(&rmVolumes, "volumes", false, "also delete the project's volumes (addon data included)")
	var renameYes bool
	rename := &cobra.Command{
		Use:   "rename <name> <new-name>",
		Short: "Rename a project: new containers come up next to the old ones, tokens and history stay",
		Long: `Rename a project on the server. Every service gets new containers of its current release; traffic
moves when they are healthy, in one nginx reload. Tokens, releases, images, logs, traces and metrics
move to the new name, and expose_to / accept_from of other projects are rewritten on the server.

Volumes are copied to their new names and the old ones removed after the switch; a service that mounts
a volume is down during its copy. A project with addons or a stopped service is refused. Databases,
buckets and anything else outside clowdr keep their names. Afterwards change "project:" in deploy.yml, and expose_to /
accept_from in the deploy.yml of other projects, or their next deploy brings the old name back.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			if !renameYes && !confirm(fmt.Sprintf("Rename project %s to %s on %s?", args[0], args[1], t.client.Server)) {
				return errors.New("aborted")
			}
			if _, err := t.client.Stream(cmd.Context(), "POST", api.Path("projects", args[0], "rename"), api.RenameProjectRequest{Name: args[1]}, a.progress); err != nil {
				return err
			}
			fmt.Fprintf(a.err, "✓ project %s is now %s\n", args[0], args[1])
			fmt.Fprintf(a.err, "  change project: in deploy.yml, and expose_to / accept_from in other projects' deploy.yml\n")
			return nil
		},
	}
	rename.Flags().BoolVarP(&renameYes, "yes", "y", false, "do not ask")
	cmd.AddCommand(create, rm, rename, &cobra.Command{
		Use:   "ls",
		Short: "List projects",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			var ps []store.Project
			if err := t.client.Do(cmd.Context(), "GET", "/v1/projects", nil, &ps); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(ps)
			}
			w := a.table("PROJECT", "DOMAINS", "ADDONS", "MEMORY")
			for _, p := range ps {
				mem := "-"
				if p.Memory > 0 {
					mem = config.FormatBytes(p.Memory)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, strings.Join(p.Domains, ","), orDash(strings.Join(p.Addons, ",")), mem)
			}
			return w.Flush()
		},
	})
	return cmd
}

func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Tokens (admin)"}
	var project, env, name string
	var admin bool
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a project token (or --admin); the secret is shown once",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			var tr api.TokenResponse
			if err := t.client.Do(cmd.Context(), "POST", "/v1/tokens", api.TokenRequest{Project: project, Env: env, Name: name, Admin: admin}, &tr); err != nil {
				return err
			}
			fmt.Fprintln(a.out, tr.Token)
			return nil
		},
	}
	create.Flags().StringVar(&project, "project", "", "project the token is limited to")
	create.Flags().StringVar(&env, "env", "", "label shown in token ls (it does not limit the token: use one server or project per env)")
	create.Flags().StringVar(&name, "name", "", "description, e.g. ci")
	create.Flags().BoolVar(&admin, "admin", false, "admin token (all projects, platform)")
	cmd.AddCommand(create, &cobra.Command{
		Use:   "ls",
		Short: "List tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			var ts []store.Token
			if err := t.client.Do(cmd.Context(), "GET", "/v1/tokens", nil, &ts); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(ts)
			}
			w := a.table("ID", "PROJECT", "ENV", "NAME", "CREATED")
			for _, tk := range ts {
				p := tk.Project
				if tk.Admin {
					p = "(admin)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", tk.ID, p, orDash(tk.Env), orDash(tk.Name), ago(tk.Created))
			}
			return w.Flush()
		},
	}, &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			return t.client.Do(cmd.Context(), "DELETE", api.Path("tokens", args[0]), nil, nil)
		},
	})
	return cmd
}

func (a *app) platformCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "platform", Short: "Server platform (admin)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "reconcile",
		Short: "Make sure nginx runs and regenerate all nginx config",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			return t.client.Do(cmd.Context(), "POST", "/v1/platform/reconcile", nil, nil)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Server overview: versions, nginx, memory and disk, certificates, projects",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(false)
			if err != nil {
				return err
			}
			var st api.PlatformStatus
			if err := t.client.Do(cmd.Context(), "GET", "/v1/platform/status", nil, &st); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(st)
			}
			a.printPlatformStatus(st)
			return nil
		},
	})
	return cmd
}

func (a *app) printPlatformStatus(st api.PlatformStatus) {
	fmt.Fprintf(a.out, "clowdrd %s, docker %s, registry %s\n", st.Version, st.Docker, st.Registry)
	if h := st.Host; h.MemTotal > 0 {
		fmt.Fprintf(a.out, "server: %s\n", hostLine(h))
	}
	n := st.Nginx
	switch {
	case n.State == "missing":
		fmt.Fprintln(a.out, "nginx:  missing (clowctl platform reconcile)")
	case n.State != "running":
		fmt.Fprintf(a.out, "nginx:  %s (clowctl platform reconcile)\n", n.State)
	default:
		fmt.Fprintf(a.out, "nginx:  running %s, %s, restarts %d\n", n.Image, strings.TrimSuffix(ago(n.Started), " ago"), n.Restarts)
	}
	if n.ConfigError != "" {
		fmt.Fprintf(a.out, "        config does not load: %s\n", n.ConfigError)
	}
	if len(st.Certs) > 0 {
		fmt.Fprintln(a.out)
		w := a.table("CERTIFICATE", "EXPIRES")
		for _, c := range st.Certs {
			exp := c.Error
			if exp == "" {
				exp = fmt.Sprintf("%s (%dd)", c.Expires.Format(time.DateOnly), int(time.Until(c.Expires).Hours()/24))
			}
			fmt.Fprintf(w, "%s\t%s\n", c.Domain, exp)
		}
		w.Flush()
	}
	fmt.Fprintln(a.out)
	w := a.table("PROJECT", "SERVICES", "RUNNING", "MEMORY / QUOTA")
	for _, p := range st.Projects {
		quota := "unlimited"
		if p.Quota > 0 {
			quota = config.FormatBytes(p.Quota)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s / %s\n", p.Name, p.Services, p.Running, human(p.Reserved), quota)
	}
	w.Flush()
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "CLI and server versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(a.out, "clowctl %s\n", a.version)
			t, err := a.target(false)
			if err != nil {
				return nil
			}
			var v api.Version
			if err := t.client.Do(cmd.Context(), "GET", "/v1/version", nil, &v); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "clowdrd %s at %s (docker %s, registry %s, token: %s)\n", v.Version, t.client.Server, v.Docker, v.Registry, v.Principal)
			return nil
		},
	}
}

// helpers

func confirm(q string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02 15:04")
}

func human(n int64) string {
	switch {
	case n == 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%dK", n>>10)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func shortTime(rfc string) string {
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return rfc
	}
	return t.Local().Format("01-02 15:04:05.000")
}

// hostLine is the server's memory, swap and disk for `top` and `platform status`. Swap is left out
// when the host has none (or the daemon predates reporting it).
func hostLine(h monitor.Host) string {
	s := fmt.Sprintf("memory %s free of %s", human(h.MemAvail), human(h.MemTotal))
	if h.SwapTotal > 0 {
		used := "0"
		if u := h.SwapTotal - h.SwapFree; u > 0 {
			used = human(u)
		}
		s += fmt.Sprintf(", swap %s used of %s", used, human(h.SwapTotal))
	}
	return s + fmt.Sprintf(", disk %s free of %s", human(h.DiskFree), human(h.DiskTotal))
}

// confirmAllowOutdated reports whether the server lists the allow-outdated feature. The field is
// sent only then: an older server refuses a deploy request with a field it does not know, and a
// failed version request confirms nothing.
func (a *app) confirmAllowOutdated(ctx context.Context, c *api.Client) bool {
	var v api.Version
	if err := c.Do(ctx, "GET", "/v1/version", nil, &v); err != nil || !slices.Contains(v.Features, api.FeatureAllowOutdated) {
		fmt.Fprintln(a.err, "! the server did not confirm it can record --allow-outdated: deploying without recording it")
		return false
	}
	return true
}
