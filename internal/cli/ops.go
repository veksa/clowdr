package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
)

func (a *app) scaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "scale <service>=<n>...",
		Short:   "Set the number of replicas (0 stops the service)",
		Example: "  clowctl scale api=3\n  clowctl scale api=2 worker=0",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			for _, arg := range args {
				name, n, ok := strings.Cut(arg, "=")
				count, err := strconv.Atoi(n)
				if !ok || err != nil {
					return fmt.Errorf("%q: expected service=N", arg)
				}
				_, err = t.client.Stream(cmd.Context(), "POST", api.Path("projects", t.project, "services", name, "scale"),
					api.ScaleRequest{Replicas: count, Author: gitAuthor()}, a.progress)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				fmt.Fprintf(a.err, "✓ %s: %d replicas\n", name, count)
			}
			return nil
		},
	}
}

func (a *app) execCmd() *cobra.Command {
	var replica int
	cmd := &cobra.Command{
		Use:   "exec <service> -- <command...>",
		Short: "Run a command in a service container (non-interactive)",
		Example: `  clowctl exec api -- ls -la /app
  clowctl exec api --replica 2 -- node -e "console.log(process.memoryUsage())"`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var resp api.ExecResponse
			err = t.client.Do(cmd.Context(), "POST", api.Path("projects", t.project, "services", args[0], "exec"),
				api.ExecRequest{Cmd: args[1:], Replica: replica}, &resp)
			if err != nil {
				return err
			}
			fmt.Fprint(a.out, resp.Output)
			if resp.ExitCode != 0 {
				return exitError(resp.ExitCode)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&replica, "replica", 1, "replica number")
	return cmd
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("command exited with code %d", int(e)) }

func (a *app) topCmd() *cobra.Command {
	var once bool
	var every time.Duration
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Live CPU and memory of the project's services and free memory/disk of the server",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			for {
				var top api.TopResponse
				if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "top"), nil, &top); err != nil {
					return err
				}
				if a.json() {
					return a.printJSON(top)
				}
				if !once {
					fmt.Fprint(a.out, "\033[H\033[2J")
				}
				h := top.Host
				if h.MemTotal > 0 {
					fmt.Fprintf(a.out, "server: memory %s free of %s, disk %s free of %s   %s\n\n",
						human(h.MemAvail), human(h.MemTotal), human(h.DiskFree), human(h.DiskTotal), time.Now().Format(time.TimeOnly))
				}
				sort.Slice(top.Services, func(i, j int) bool { return top.Services[i].Name < top.Services[j].Name })
				w := a.table("SERVICE", "CONTAINER", "STATE", "CPU", "MEM", "LIMIT", "RESTARTS")
				for _, s := range top.Services {
					if len(s.Containers) == 0 {
						fmt.Fprintf(w, "%s\t-\t%s\t\t\t\t\n", s.Name, map[bool]string{true: "stopped", false: "-"}[s.Stopped])
					}
					for _, c := range s.Containers {
						fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%s\t%s\t%d\n", s.Name, replicaOf(c.Name), c.State, c.CPU, human(c.MemUsage), config.FormatBytes(s.Memory), c.Restarts)
					}
				}
				w.Flush()
				if once {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(every):
				}
			}
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "print once and exit")
	cmd.Flags().DurationVar(&every, "every", 2*time.Second, "refresh interval")
	return cmd
}

func (a *app) volumeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "volume", Short: "Named volumes of the project (never removed automatically)"}
	var yes bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a volume and its data",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Delete volume %s of %s with all its data?", args[0], t.project)) {
				return errors.New("aborted")
			}
			return t.client.Do(cmd.Context(), "DELETE", api.Path("projects", t.project, "volumes", args[0]), nil, nil)
		},
	}
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List volumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var vs []deploy.Volume
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "volumes"), nil, &vs); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(vs)
			}
			w := a.table("VOLUME", "USED BY", "CREATED")
			for _, v := range vs {
				fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, orDash(strings.Join(v.UsedBy, ", ")), v.Created)
			}
			return w.Flush()
		},
	}, rm)
	return cmd
}

func (a *app) addonCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "addon", Short: "The project's own addon instances (redis)"}
	var volumes, yes bool
	rm := &cobra.Command{
		Use:   "rm <addon>",
		Short: "Remove an addon instance (data volume only with --volumes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Remove %s of %s?", args[0], t.project)) {
				return errors.New("aborted")
			}
			path := api.Path("projects", t.project, "addons", args[0])
			if volumes {
				path += "?volumes=1"
			}
			return t.client.Do(cmd.Context(), "DELETE", path, nil, nil)
		},
	}
	rm.Flags().BoolVar(&volumes, "volumes", false, "also delete the data volume")
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List addons",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var as []deploy.Addon
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "addons"), nil, &as); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(as)
			}
			w := a.table("ADDON", "IMAGE", "STATE", "MEMORY")
			for _, ad := range as {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ad.Name, ad.Image, ad.State, config.FormatBytes(ad.Memory))
			}
			return w.Flush()
		},
	}, &cobra.Command{
		Use:     "exec <addon> -- <command...>",
		Short:   "Run a command in the addon container",
		Example: "  clowctl addon exec redis -- redis-cli INFO memory",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var resp api.ExecResponse
			if err := t.client.Do(cmd.Context(), "POST", api.Path("projects", t.project, "addons", args[0], "exec"), api.ExecRequest{Cmd: args[1:]}, &resp); err != nil {
				return err
			}
			fmt.Fprint(a.out, resp.Output)
			if resp.ExitCode != 0 {
				return exitError(resp.ExitCode)
			}
			return nil
		},
	}, rm)
	return cmd
}
