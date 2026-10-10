package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
)

// profileDir is where clowdr-node keeps pid files and writes profiles (sdk/node/src/profile.ts).
const profileDir = "/tmp/clowdr-node"

func (a *app) profileCmd() *cobra.Command {
	var seconds, replica int
	var out string
	cmd := &cobra.Command{
		Use:   "profile <service>",
		Short: "CPU profile of a Node service (clowdr-node 0.4+): open the .cpuprofile in Chrome DevTools",
		Long: `Every process of the service that called installTracing() profiles itself for --seconds with
the built-in inspector (all workers of a cluster, every replica unless --replica), and the
profiles are downloaded as <service>-r<replica>-<pid>-<time>.cpuprofile. No endpoint is involved:
the request goes through exec and a signal (SIGUSR2). Open a file in Chrome DevTools →
Performance → Load profile.`,
		Example: `  clowctl profile server --seconds 30
  clowctl profile server --replica 2 -o /tmp`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			if seconds < 1 || seconds > 300 {
				return errors.New("--seconds: 1..300")
			}
			service := args[0]
			replicas := []int{replica}
			if replica == 0 {
				var st api.ServiceStatus
				if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services", service), nil, &st); err != nil {
					return err
				}
				replicas = nil
				for i, c := range st.Containers {
					if c.State == "running" {
						replicas = append(replicas, i+1)
					}
				}
				if len(replicas) == 0 {
					return fmt.Errorf("%s has no running container", service)
				}
			}
			exec := func(ctx context.Context, r int, script string) (api.ExecResponse, error) {
				var resp api.ExecResponse
				err := t.client.Do(ctx, "POST", api.Path("projects", t.project, "services", service, "exec"),
					api.ExecRequest{Cmd: []string{"sh", "-c", script}, Replica: r}, &resp)
				return resp, err
			}

			// Ask every process of every replica first, so they all profile the same window.
			pids := map[int][]string{}
			start := fmt.Sprintf(`d=%s; ls $d/*.pid >/dev/null 2>&1 || { echo NO_PROFILER; exit 3; }
rm -f $d/*.cpuprofile $d/*.part; echo %d > $d/request
for f in $d/*.pid; do p=$(basename "$f" .pid); if kill -USR2 "$p" 2>/dev/null; then echo "$p"; else rm -f "$f"; fi; done`, profileDir, seconds)
			for _, r := range replicas {
				resp, err := exec(cmd.Context(), r, start)
				if err != nil {
					return err
				}
				if strings.Contains(resp.Output, "NO_PROFILER") {
					return fmt.Errorf("replica %d does not answer clowctl profile: the service needs clowdr-node 0.4+ with installTracing()", r)
				}
				pids[r] = strings.Fields(resp.Output)
				fmt.Fprintf(a.out, "replica %d: profiling %d process(es) for %ds\n", r, len(pids[r]), seconds)
			}

			select {
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			case <-time.After(time.Duration(seconds)*time.Second + 2*time.Second):
			}

			stamp := time.Now().Format("20060102-150405")
			var saved int
			for _, r := range replicas {
				for _, pid := range pids[r] {
					if _, err := strconv.Atoi(pid); err != nil {
						continue
					}
					file := filepath.Join(out, fmt.Sprintf("%s-r%d-%s-%s.cpuprofile", service, r, pid, stamp))
					var body string
					for try := 0; try < 20; try++ {
						resp, err := exec(cmd.Context(), r, fmt.Sprintf(`cat %s/%s.cpuprofile 2>/dev/null`, profileDir, pid))
						if err != nil {
							return err
						}
						if resp.ExitCode == 0 && strings.HasPrefix(strings.TrimSpace(resp.Output), "{") {
							body = resp.Output
							break
						}
						time.Sleep(time.Second)
					}
					if body == "" {
						fmt.Fprintf(a.out, "replica %d pid %s: no profile (the process may have restarted)\n", r, pid)
						continue
					}
					if err := os.WriteFile(file, []byte(strings.TrimSpace(body)), 0o644); err != nil {
						return err
					}
					fmt.Fprintf(a.out, "%s (%d KB)\n", file, len(body)/1024)
					saved++
				}
			}
			if saved == 0 {
				return errors.New("no profile was written")
			}
			fmt.Fprintln(a.out, "Open in Chrome DevTools → Performance → Load profile.")
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVar(&seconds, "seconds", 30, "how long to profile, 1..300")
	f.IntVar(&replica, "replica", 0, "only this replica (default: every running replica)")
	f.StringVarP(&out, "output", "O", ".", "directory to save the profiles in")
	return cmd
}
