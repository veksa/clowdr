package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/monitor"
)

func (a *app) eventsCmd() *cobra.Command {
	var follow, platform bool
	var since string
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Deploys, crashes, OOM, restart loops, failing health checks, 5xx spikes",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(!platform)
			if err != nil {
				return err
			}
			project := t.project
			if platform {
				project = logs.Platform
			}
			q := url.Values{}
			if since != "" {
				q.Set("since", since)
			}
			if follow {
				q.Set("follow", "1")
			}
			body, err := t.client.Raw(cmd.Context(), "GET", api.Path("projects", project, "events")+"?"+q.Encode())
			if err != nil {
				return err
			}
			defer body.Close()
			dec := json.NewDecoder(body)
			for {
				var e monitor.Event
				if err := dec.Decode(&e); err != nil {
					return nil
				}
				if a.json() {
					json.NewEncoder(a.out).Encode(e)
					continue
				}
				svc := e.Service
				if svc == "" {
					svc = "-"
				}
				fmt.Fprintf(a.out, "%s  %-5s  %-12s %-10s %s\n", e.Time.Local().Format(time.DateTime), e.Severity, e.Kind, svc, strings.ReplaceAll(e.Message, "\n", " "))
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new events")
	cmd.Flags().BoolVar(&platform, "platform", false, "host events: nginx, disk, memory (admin)")
	cmd.Flags().StringVar(&since, "since", "", "e.g. 24h, 7d")
	return cmd
}
