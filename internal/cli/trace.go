package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/trace"
)

// traceQuery holds the filters shared by the trace subcommands.
type traceQuery struct {
	service, since, until, query, user, source string
}

func (q *traceQuery) flags(cmd *cobra.Command, since string) {
	f := cmd.Flags()
	f.StringVar(&q.service, "service", "", "only this service (default: every service of the project)")
	f.StringVar(&q.since, "since", since, "only newer than, e.g. 2h, 7d or an RFC 3339 time")
	f.StringVar(&q.until, "until", "", "only older than, e.g. 1h")
	f.StringVarP(&q.query, "query", "q", "", `substring anywhere in a record, e.g. '"orderId":88'`)
	f.StringVar(&q.user, "user", "", "user id")
	f.StringVar(&q.source, "source", "", "substring of what started the trace, e.g. job: or http:POST")
}

func (q *traceQuery) values() url.Values {
	v := url.Values{}
	for k, s := range map[string]string{"service": q.service, "since": q.since, "until": q.until, "q": q.query, "user": q.user, "source": q.source} {
		if s != "" {
			v.Set(k, s)
		}
	}
	return v
}

func (a *app) traceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace",
		Short: "Traces services write to stdout: list, open one, records, errors",
		Long: `Every unit of work a service does (a request, a job run) is one trace: the model calls with
prompts and answers, outgoing HTTP, SQL, log lines, the outcome. Services write the records to
stdout (docs/tracing.md, the clowdr-node package); clowdr keeps them in a separate stream.`,
		Example: `  clowctl trace ls --since 2h --errors
  clowctl trace ls -q '"orderId":88'
  clowctl trace show <traceId> --verbose
  clowctl trace errors --since 30m
  clowctl trace records --type llm -f`,
	}
	cmd.AddCommand(a.traceLsCmd(), a.traceShowCmd(), a.traceLastCmd(), a.traceRecordsCmd(false), a.traceRecordsCmd(true))
	return cmd
}

func (a *app) traceList(ctx context.Context, t *target, q *traceQuery, errorsOnly bool, limit int) ([]trace.Summary, error) {
	v := q.values()
	if errorsOnly {
		v.Set("errors", "1")
	}
	v.Set("limit", strconv.Itoa(limit))
	var ss []trace.Summary
	err := t.client.Do(ctx, "GET", api.Path("projects", t.project, "traces")+"?"+v.Encode(), nil, &ss)
	return ss, err
}

func (a *app) traceLsCmd() *cobra.Command {
	var q traceQuery
	var errorsOnly bool
	var limit int
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Traces rolled up, newest first",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			ss, err := a.traceList(cmd.Context(), t, &q, errorsOnly, limit)
			if err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(ss)
			}
			fmt.Fprintln(a.out, trace.FormatSummaries(ss))
			return nil
		},
	}
	q.flags(cmd, "24h")
	cmd.Flags().BoolVar(&errorsOnly, "errors", false, "only traces with an error")
	cmd.Flags().IntVar(&limit, "limit", 50, "at most this many traces")
	return cmd
}

func (a *app) printTrace(ctx context.Context, t *target, id string, q *traceQuery, o trace.FormatOptions) error {
	v := url.Values{}
	if q.service != "" {
		v.Set("service", q.service)
	}
	if q.since != "" {
		v.Set("since", q.since)
	}
	var d api.TraceDetail
	if err := t.client.Do(ctx, "GET", api.Path("projects", t.project, "traces", id)+"?"+v.Encode(), nil, &d); err != nil {
		return err
	}
	if a.json() {
		return a.printJSON(d)
	}
	fmt.Fprintln(a.out, trace.FormatTrace(d.Summary, d.Records, o))
	return nil
}

func (a *app) traceShowCmd() *cobra.Command {
	var q traceQuery
	var o trace.FormatOptions
	cmd := &cobra.Command{
		Use:   "show <traceId>",
		Short: "One trace in full, top to bottom",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			return a.printTrace(cmd.Context(), t, args[0], &q, o)
		},
	}
	cmd.Flags().StringVar(&q.service, "service", "", "only this service")
	cmd.Flags().StringVar(&q.since, "since", "7d", "how far back to look for it")
	cmd.Flags().BoolVar(&o.Verbose, "verbose", false, "every message of each model call")
	cmd.Flags().IntVar(&o.MaxChars, "max-chars", 4000, "per-field budget; longer fields are cut with a marker")
	return cmd
}

func (a *app) traceLastCmd() *cobra.Command {
	var q traceQuery
	var o trace.FormatOptions
	cmd := &cobra.Command{
		Use:   "last",
		Short: "The newest matching trace in full (e.g. --user 42 right after a bug report)",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			ss, err := a.traceList(cmd.Context(), t, &q, false, 1)
			if err != nil {
				return err
			}
			if len(ss) == 0 {
				return errors.New("no traces match")
			}
			return a.printTrace(cmd.Context(), t, ss[0].TraceID, &q, o)
		},
	}
	q.flags(cmd, "24h")
	cmd.Flags().BoolVar(&o.Verbose, "verbose", false, "every message of each model call")
	cmd.Flags().IntVar(&o.MaxChars, "max-chars", 4000, "per-field budget")
	return cmd
}

// traceRecordsCmd is `records` or, with failures, `errors`: failed calls and warn/error lines.
func (a *app) traceRecordsCmd(failures bool) *cobra.Command {
	var q traceQuery
	var types, levels, traceID string
	var tail int
	var follow bool
	var o trace.FormatOptions
	use, short, since := "records", "Raw records across traces, by type, level or text", ""
	if failures {
		use, short, since = "errors", "Failed calls and warn/error records", "2h"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			v := q.values()
			for k, s := range map[string]string{"type": types, "level": levels, "trace": traceID} {
				if s != "" {
					v.Set(k, s)
				}
			}
			if failures {
				v.Set("failures", "1")
			}
			if cmd.Flags().Changed("tail") || q.since == "" {
				v.Set("tail", strconv.Itoa(tail))
			}
			if follow {
				v.Set("follow", "1")
			}
			resp, err := t.client.Raw(cmd.Context(), "GET", api.Path("projects", t.project, "trace-records")+"?"+v.Encode())
			if err != nil {
				return err
			}
			defer resp.Close()
			dec := json.NewDecoder(resp)
			var batch []trace.Record
			for {
				var r trace.Record
				if err := dec.Decode(&r); err != nil {
					break
				}
				if a.json() {
					json.NewEncoder(a.out).Encode(r)
					continue
				}
				if follow {
					fmt.Fprintln(a.out, trace.FormatRecords([]trace.Record{r}, o))
					continue
				}
				batch = append(batch, r)
			}
			if !a.json() && !follow {
				fmt.Fprintln(a.out, trace.FormatRecords(batch, o))
			}
			return nil
		},
	}
	q.flags(cmd, since)
	f := cmd.Flags()
	f.StringVar(&types, "type", "", "record types, comma-separated: llm,http,job,log,storage,db,note,socket")
	f.StringVar(&levels, "level", "", "levels, comma-separated: debug,info,warn,error")
	f.StringVar(&traceID, "trace", "", "only this trace")
	f.IntVar(&tail, "tail", 100, "last N records")
	f.BoolVarP(&follow, "follow", "f", false, "stream new records")
	f.IntVar(&o.MaxChars, "max-chars", 1500, "per-field budget")
	return cmd
}
