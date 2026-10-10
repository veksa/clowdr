package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
)

func (a *app) accessCmd() *cobra.Command {
	var follow bool
	var since, until, status, path, top, export string
	var tail, limit int
	cmd := &cobra.Command{
		Use:   "access [domain]",
		Short: "nginx access log of the project: filter, stream, summaries",
		Example: `  clowctl access api.example.com --since 24h --status 5xx
  clowctl access api.example.com -f --path /api
  clowctl access --top paths --since 1h
  clowctl access example.com --since 7d --export combined > access.log   # for GoAccess`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			q := url.Values{}
			if len(args) == 1 {
				q.Set("domain", args[0])
			}
			for k, v := range map[string]string{"since": since, "until": until, "status": status, "path": path, "top": top, "export": export} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if follow {
				q.Set("follow", "1")
			}
			if cmd.Flags().Changed("tail") {
				q.Set("tail", strconv.Itoa(tail))
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			base := api.Path("projects", t.project, "access") + "?" + q.Encode()
			ctx := cmd.Context()

			switch {
			case top != "":
				var res api.TopResult
				if err := t.client.Do(ctx, "GET", base, nil, &res); err != nil {
					return err
				}
				if a.json() {
					return a.printJSON(res)
				}
				w := a.table("COUNT", "SHARE", top)
				shown := 0
				for _, r := range res.Rows {
					key := r.Key
					if key == "" {
						key = "-"
					}
					shown += r.Count
					fmt.Fprintf(w, "%d\t%.1f%%\t%s\n", r.Count, 100*float64(r.Count)/float64(max(res.Total, 1)), key)
				}
				if rest := res.Total - shown; rest > 0 {
					fmt.Fprintf(w, "%d\t%.1f%%\t(other)\n", rest, 100*float64(rest)/float64(res.Total))
				}
				return w.Flush()
			case export != "":
				var s string
				if err := t.client.Do(ctx, "GET", base, nil, &s); err != nil {
					return err
				}
				fmt.Fprint(a.out, s)
				return nil
			}
			resp, err := t.client.Raw(ctx, "GET", base)
			if err != nil {
				return err
			}
			defer resp.Close()
			dec := json.NewDecoder(resp)
			for {
				var e api.AccessEntry
				if err := dec.Decode(&e); err != nil {
					return nil
				}
				if a.json() {
					json.NewEncoder(a.out).Encode(e)
					continue
				}
				fmt.Fprintf(a.out, "%s %3d %6.3fs %-15s %s %s%s  %s\n", shortTime(e.Time), e.Status, e.RT, e.IP, e.Method, e.Host, e.URI, e.Country)
			}
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "stream new requests")
	f.StringVar(&since, "since", "", "e.g. 24h, 7d")
	f.StringVar(&until, "until", "", "e.g. 1h")
	f.StringVar(&status, "status", "", "status or class: 404, 5xx")
	f.StringVar(&path, "path", "", "path prefix")
	f.StringVar(&top, "top", "", "summary: paths|ips|statuses|countries|referers|domains")
	f.StringVar(&export, "export", "", "combined: nginx combined format for GoAccess")
	f.IntVar(&tail, "tail", 100, "last N requests")
	f.IntVar(&limit, "limit", 0, "rows in --top")
	return cmd
}
