package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/metric"
)

func (a *app) metricsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Metrics services write to stdout and clowdr samples (CPU, memory): list, aggregate",
		Long: `Services write one point per series per minute to stdout (docs/metrics.md, the clowdr-node
package): counters as increases, gauges as values, histograms as bucket counts. clowdr adds the
CPU and memory of every container. Points are kept in a separate stream.`,
		Example: `  clowctl metrics ls
  clowctl metrics show task_runs --since 24h --step 1h --by outcome
  clowctl metrics show queue_depth --since 6h --step 10m --agg max
  clowctl metrics show task_duration_seconds --agg p95 --where step=render
  clowctl metrics show container_memory_bytes --by service --step 5m --since 2h`,
	}
	cmd.AddCommand(a.metricsLsCmd(), a.metricsShowCmd())
	return cmd
}

func (a *app) metricsLsCmd() *cobra.Command {
	var service, since string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Metric names, kinds, labels and last values",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			v := url.Values{"since": {since}}
			if service != "" {
				v.Set("service", service)
			}
			var infos []metric.Info
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "metrics")+"?"+v.Encode(), nil, &infos); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(infos)
			}
			w := a.table("NAME", "KIND", "SERVICE", "SERIES", "LAST", "LABELS")
			for _, in := range infos {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", in.Name, in.Kind, in.Service, in.Series, num(in.Last), labelSummary(in.Labels))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only this service")
	cmd.Flags().StringVar(&since, "since", "24h", "look this far back")
	return cmd
}

func (a *app) metricsShowCmd() *cobra.Command {
	var service, since, until, step, by, agg, where string
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "One metric over time: rows are time buckets, columns the series of --by",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			v := url.Values{}
			for k, s := range map[string]string{"service": service, "since": since, "until": until, "step": step, "by": by, "agg": agg, "where": where} {
				if s != "" {
					v.Set(k, s)
				}
			}
			var res api.MetricResult
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "metrics", args[0])+"?"+v.Encode(), nil, &res); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(res)
			}
			return a.printMetric(res)
		},
	}
	f := cmd.Flags()
	f.StringVar(&service, "service", "", "only this service")
	f.StringVar(&since, "since", "24h", "e.g. 2h, 7d or an RFC 3339 time")
	f.StringVar(&until, "until", "", "e.g. 1h")
	f.StringVar(&step, "step", "1h", "bucket size, at least 1m")
	f.StringVar(&by, "by", "", "labels to split by, comma-separated (also: service, replica)")
	f.StringVar(&agg, "agg", "", "counter: sum|rate; gauge: max|last|min|avg|sum; histogram: p95|p50|p90|p99|avg|count|max|sum")
	f.StringVar(&where, "where", "", "label filters, e.g. step=render,outcome=failed")
	return cmd
}

func (a *app) printMetric(res api.MetricResult) error {
	fmt.Fprintf(a.out, "%s (%s, step %s%s)\n", res.Name, res.Kind, res.Step, aggNote(res))
	if len(res.Series) == 0 {
		fmt.Fprintln(a.out, "(no points)")
		return nil
	}
	headers := []string{"TIME"}
	for _, s := range res.Series {
		headers = append(headers, seriesName(s.Labels))
	}
	rows := map[string][]string{}
	for i, s := range res.Series {
		for _, p := range s.Points {
			r := rows[p.T]
			if r == nil {
				r = make([]string, len(res.Series))
				for j := range r {
					r[j] = "-"
				}
				rows[p.T] = r
			}
			r[i] = num(p.V)
		}
	}
	times := make([]string, 0, len(rows))
	for t := range rows {
		times = append(times, t)
	}
	sort.Strings(times)
	w := a.table(headers...)
	for _, t := range times {
		fmt.Fprintf(w, "%s\t%s\n", shortTime(t), strings.Join(rows[t], "\t"))
	}
	return w.Flush()
}

func aggNote(res api.MetricResult) string {
	if res.Agg != "" {
		return ", " + res.Agg
	}
	switch res.Kind {
	case metric.Counter:
		return ", sum"
	case metric.Gauge:
		return ", max"
	case metric.Histogram:
		return ", p95"
	}
	return ""
}

func seriesName(labels map[string]string) string {
	if len(labels) == 0 {
		return "VALUE"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v := labels[k]
		if v == "" {
			v = "-"
		}
		parts[i] = v
	}
	return strings.Join(parts, ",")
}

func labelSummary(labels map[string][]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		vs := labels[k]
		s := strings.Join(vs, "|")
		if len(s) > 40 {
			s = s[:40] + "…"
		}
		parts[i] = k + "=" + s
	}
	return strings.Join(parts, " ")
}

func num(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}
