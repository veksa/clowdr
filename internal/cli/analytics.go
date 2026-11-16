package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/veksa/clowdr/internal/analytics"
	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/config"
)

func (a *app) analyticsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "analytics",
		Aliases: []string{"an"},
		Short:   "Product analytics services write to stdout: trends, funnels, retention, installs, digest",
		Long: `Services write one record per product event to stdout (docs/analytics.md). clowdr keeps
the raw records for the hot window (platform.yml logs.analytics), links devices to persons,
decides installs and reinstalls, computes daily rollups, archives everything to the project's
S3 bucket and sends a daily digest to Telegram. Test persons are left out unless --include-test.`,
		Example: `  clowctl analytics trends episode_viewed --since 14d
  clowctl analytics trends any --math dau --by pf
  clowctl analytics funnel '$first_open' user_signed_up episode_viewed --window 7d
  clowctl analytics retention --period week --by first_source
  clowctl analytics installs --since 30d
  clowctl analytics person 870
  clowctl analytics digest --dry-run
  clowctl analytics check records.ndjson
  clowctl analytics import history.ndjson.zst`,
	}
	cmd.AddCommand(
		a.anTrendsCmd(), a.anFunnelCmd(), a.anRetentionCmd(), a.anStickinessCmd(), a.anLifecycleCmd(),
		a.anPathsCmd(), a.anPersonCmd(), a.anEventsCmd(), a.anInstallsCmd(), a.anCatalogCmd(),
		a.anStatusCmd(), a.anArchiveCmd(), a.anDigestCmd(), a.anCheckCmd(), a.anSecretCmd(), a.anRebuildCmd(),
		a.anImportCmd(),
	)
	return cmd
}

// anFlags are the flags every query takes.
type anFlags struct {
	since, until string
	breakdown    []string
	where        []string
	includeTest  bool
}

func (f *anFlags) add(cmd *cobra.Command, since string, breakdown bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.since, "since", since, "e.g. 7d, 2026-11-01 or an RFC 3339 time")
	fl.StringVar(&f.until, "until", "", "e.g. 1d or 2026-11-10")
	fl.StringArrayVar(&f.where, "where", nil, "filter on a dimension, p.<prop>, person.<prop> or first_*: k=v, k!=v, k>n, k>=n, k<n, k<=n; k=a|b (or k=a,b) is one of, k!=a|b none of; repeatable, all must hold, the same key with = twice is one of")
	fl.BoolVar(&f.includeTest, "include-test", false, "count test persons too")
	if breakdown {
		fl.StringArrayVarP(&f.breakdown, "by", "b", nil, "split by a dimension, p.<prop>, person.<prop> or first_* (also --breakdown); twice for two keys")
	}
	fl.SetNormalizeFunc(anFlagAliases)
}

// anFlagAliases takes the long spellings too: --breakdown for --by, --tail for --limit.
func anFlagAliases(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	switch name {
	case "breakdown":
		name = "by"
	case "tail":
		name = "limit"
	}
	return pflag.NormalizedName(name)
}

func (f *anFlags) spec(kind string) (analytics.Spec, error) {
	s := analytics.Spec{Kind: kind, Since: f.since, Until: f.until, IncludeTest: f.includeTest}
	if len(f.breakdown) > 2 {
		return s, fmt.Errorf("--by: at most two keys")
	}
	s.Breakdown = strings.Join(f.breakdown, ",")
	// Plain k=v and k!=v with distinct keys travel as the where map, which every server with
	// analytics reads; anything else (comparisons, lists, a repeated key) as filters.
	simple := map[string]string{}
	plain := true
	for _, w := range f.where {
		if !strings.ContainsAny(w, "=<>") || strings.IndexAny(w, "=<>") == 0 || strings.HasPrefix(w, "!") {
			return s, fmt.Errorf("--where %q: expected k=v, k!=v, k>n, k>=n, k<n or k<=n", w)
		}
		k, v, _ := strings.Cut(w, "=")
		if strings.ContainsAny(k, "<>") || strings.ContainsAny(v, "|,") || strings.HasPrefix(v, "=") {
			plain = false
		}
		if _, dup := simple[k]; dup {
			plain = false
		}
		simple[k] = v // "k!" (from k!=v) is the server's "not equal"
	}
	if len(f.where) > 0 {
		if plain {
			s.Where = simple
		} else {
			s.Filters = append([]string(nil), f.where...)
		}
	}
	return s, nil
}

func (a *app) anQuery(ctx context.Context, s analytics.Spec) error {
	t, err := a.target(true)
	if err != nil {
		return err
	}
	var res analytics.Result
	if err := t.client.Do(ctx, "POST", api.Path("projects", t.project, "analytics", "query"), s, &res); err != nil {
		return err
	}
	if a.json() {
		return a.printJSON(res)
	}
	return a.printAnalytics(&res)
}

func (a *app) anQueryCmd(use, short string, args cobra.PositionalArgs, since string, breakdown bool, build func(*analytics.Spec, []string), more func(*cobra.Command)) *cobra.Command {
	var f anFlags
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, _, _ := strings.Cut(use, " ")
			s, err := f.spec(kind)
			if err != nil {
				return err
			}
			build(&s, args)
			return a.anQuery(cmd.Context(), s)
		},
	}
	f.add(cmd, since, breakdown)
	if more != nil {
		more(cmd)
	}
	return cmd
}

func (a *app) anTrendsCmd() *cobra.Command {
	var math, prop, interval string
	var compare bool
	return a.anQueryCmd("trends <event...>", "Events over time (any = any activity): counts, uniques, DAU/WAU/MAU, sums",
		cobra.MinimumNArgs(1), "14d", true, func(s *analytics.Spec, args []string) {
			s.Events, s.Math, s.Prop, s.Interval, s.Compare = args, math, prop, interval, compare
		}, func(cmd *cobra.Command) {
			f := cmd.Flags()
			f.StringVar(&math, "math", "", "total|uniques|dau|wau|mau|per_user_avg|sum|avg|p50|p75|p90|p95|p99 (default total)")
			f.StringVar(&prop, "prop", "", "the numeric property of sum/avg/p50/p90")
			f.StringVar(&interval, "interval", "", "hour|day|week|month (default day)")
			f.BoolVar(&compare, "compare", false, "the previous period too")
		})
}

func (a *app) anFunnelCmd() *cobra.Command {
	var window, order string
	return a.anQueryCmd("funnel <step...>", "Conversion through steps (event or event:k=v,k=v), per actor",
		cobra.MinimumNArgs(2), "30d", true, func(s *analytics.Spec, args []string) {
			s.Events, s.Window, s.Order = args, window, order
		}, func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&window, "window", "", "conversion window, e.g. 1h or 7d (default 14d)")
			cmd.Flags().StringVar(&order, "order", "", "ordered (default) | strict | any")
		})
}

func (a *app) anRetentionCmd() *cobra.Command {
	var start, ret, period string
	return a.anQueryCmd("retention", "Cohorts by first activity (or --start) and how many come back",
		cobra.NoArgs, "56d", true, func(s *analytics.Spec, _ []string) {
			s.Start, s.Return, s.Period = start, ret, period
		}, func(cmd *cobra.Command) {
			f := cmd.Flags()
			f.StringVar(&start, "start", "", "cohort event (default: first seen)")
			f.StringVar(&ret, "return", "", "returning event (default: any activity)")
			f.StringVar(&period, "period", "", "day|week|month (default week)")
		})
}

func (a *app) anStickinessCmd() *cobra.Command {
	var event string
	return a.anQueryCmd("stickiness [event]", "How many days of the window actors were active (default: any activity)",
		cobra.MaximumNArgs(1), "30d", false, func(s *analytics.Spec, args []string) {
			s.Event = firstOr(args, firstOr(nonEmpty(event), "any"))
		}, func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&event, "event", "", "the event (or as the argument)")
		})
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func (a *app) anLifecycleCmd() *cobra.Command {
	var interval, event string
	return a.anQueryCmd("lifecycle [event]", "New, returning, resurrecting and dormant actors per period",
		cobra.MaximumNArgs(1), "30d", false, func(s *analytics.Spec, args []string) {
			s.Event, s.Interval = firstOr(args, firstOr(nonEmpty(event), "any")), interval
		}, func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&interval, "interval", "", "day|week|month (default week)")
			cmd.Flags().StringVar(&event, "event", "", "the event (or as the argument; default any)")
		})
}

func (a *app) anPathsCmd() *cobra.Command {
	var from, to string
	var depth, limit int
	return a.anQueryCmd("paths", "Screen paths ($pageview screen_name): from or to a screen, or the commonest steps",
		cobra.NoArgs, "7d", false, func(s *analytics.Spec, _ []string) {
			s.From, s.To, s.Depth, s.Limit = from, to, depth, limit
		}, func(cmd *cobra.Command) {
			f := cmd.Flags()
			f.StringVar(&from, "from", "", "paths starting at this screen")
			f.StringVar(&to, "to", "", "paths ending at this screen")
			f.IntVar(&depth, "depth", 0, "screens per path, 3 to 5 (default 3)")
			f.IntVar(&limit, "limit", 0, "paths and screens to show, at most 1000 (default 20)")
		})
}

func (a *app) anPersonCmd() *cobra.Command {
	var limit int
	return a.anQueryCmd("person <user>", "One person: properties, devices, install source and recent events",
		cobra.ExactArgs(1), "30d", false, func(s *analytics.Spec, args []string) {
			s.User, s.Limit = args[0], limit
		}, func(cmd *cobra.Command) {
			cmd.Flags().IntVar(&limit, "events", 0, "events to show (default 100)")
		})
}

func (a *app) anEventsCmd() *cobra.Command {
	var event, user, device string
	var limit int
	return a.anQueryCmd("events", "Raw records, newest first, by event, user or device",
		cobra.NoArgs, "1d", false, func(s *analytics.Spec, _ []string) {
			s.Event, s.User, s.Device, s.Limit = event, user, device, limit
		}, func(cmd *cobra.Command) {
			f := cmd.Flags()
			f.StringVar(&event, "event", "", "only this event")
			f.StringVar(&user, "user", "", "only this user id")
			f.StringVar(&device, "device", "", "only this device id")
			f.IntVar(&limit, "limit", 0, "records to show (default 50; also --tail)")
		})
}

func (a *app) anInstallsCmd() *cobra.Command {
	var by string
	return a.anQueryCmd("installs", "Installs, reinstalls and web first visits by source, with D1/D7/D30 retention",
		cobra.NoArgs, "30d", false, func(s *analytics.Spec, _ []string) {
			s.Breakdown = by
		}, func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&by, "by", "", "source (default) | campaign | store | cc | pf | app")
		})
}

func (a *app) anCatalogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "catalog",
		Short: "Every event seen: counts, sources, dimensions and properties",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.anQuery(cmd.Context(), analytics.Spec{Kind: "catalog"})
		},
	}
}

func firstOr(args []string, def string) string {
	if len(args) > 0 {
		return args[0]
	}
	return def
}

func (a *app) anStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Records received, the hot window, rollups, the archive and the digest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var s analytics.Status
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "analytics", "status"), nil, &s); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(s)
			}
			w := a.table("", "")
			row := func(k, v string) { fmt.Fprintf(w, "%s\t%s\n", k, v) }
			row("configured", fmt.Sprint(s.Configured))
			row("secrets", strings.Join(s.Secrets, ", "))
			row("services", strings.Join(s.Services, ", "))
			row("records", fmt.Sprintf("%d last hour, %d last 24h, last %s", s.LastHour, s.Last24h, dash(s.LastRecord)))
			row("raw window", fmt.Sprintf("%s → now, %s on disk", shortTime(s.Window.Since), humanBytes(s.DiskBytes)))
			row("rollups", fmt.Sprintf("%d days, computed %s", s.Rollups, dash(s.Computed)))
			ar := "off"
			if s.Archive.On {
				ar = fmt.Sprintf("last day %s, last ok %s", dash(s.Archive.LastDay), dash(s.Archive.LastOK))
				if s.Archive.Snapshot != "" {
					ar += ", snapshot " + s.Archive.Snapshot
				}
			}
			row("archive", ar)
			if s.Archive.Problem != "" {
				row("", "⚠ "+s.Archive.Problem)
			}
			if len(s.Archive.Missing) > 0 {
				row("", "⚠ not uploaded: "+strings.Join(s.Archive.Missing, ", "))
			}
			if s.Archive.LastError != "" {
				row("", fmt.Sprintf("⚠ %s: %s", s.Archive.LastErrorAt, s.Archive.LastError))
			}
			row("digest", "last sent for "+dash(s.Digest))
			if len(s.Dropped) > 0 {
				var parts []string
				for k, v := range s.Dropped {
					parts = append(parts, fmt.Sprintf("%s %s", k, num(v)))
				}
				sort.Strings(parts)
				row("dropped 24h", strings.Join(parts, ", "))
			}
			if s.IndexDrops > 0 {
				row("index drops", fmt.Sprint(s.IndexDrops))
			}
			return w.Flush()
		},
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (a *app) anArchiveCmd() *cobra.Command {
	var since, until string
	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Presigned URLs of the archived raw days, for DuckDB (read_ndjson) or any tool",
		Example: `  clowctl analytics archive --since 2026-10-01 --until 2026-10-31 > urls.txt
  duckdb -c "select e, count(*) from read_ndjson([$(sed "s/.*/'&'/" urls.txt | paste -sd,)]) group by 1"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			v := url.Values{"since": {since}}
			if until != "" {
				v.Set("until", until)
			}
			var res api.AnalyticsArchive
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "analytics", "archive")+"?"+v.Encode(), nil, &res); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(res)
			}
			for _, u := range res.URLs {
				fmt.Fprintln(a.out, u)
			}
			fmt.Fprintf(a.err, "%d files, the links expire %s\n", len(res.URLs), res.Expires)
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "7d", "first day")
	cmd.Flags().StringVar(&until, "until", "", "last day (default: now)")
	return cmd
}

func (a *app) anDigestCmd() *cobra.Command {
	var day string
	var dry bool
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "The daily digest (yesterday by default): print it, and send it to Telegram unless --dry-run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := a.target(true)
			if err != nil {
				return err
			}
			v := url.Values{}
			if day != "" {
				v.Set("day", day)
			}
			if dry {
				v.Set("dry_run", "1")
			}
			var res api.AnalyticsDigest
			if err := t.client.Do(cmd.Context(), "POST", api.Path("projects", t.project, "analytics", "digest")+"?"+v.Encode(), nil, &res); err != nil {
				return err
			}
			if a.json() {
				return a.printJSON(res)
			}
			fmt.Fprintln(a.out, res.Text)
			if res.Sent {
				fmt.Fprintln(a.err, "✓ sent")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&day, "day", "", "the day, e.g. 2026-11-10 (default: yesterday, UTC)")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "only print it")
	return cmd
}

func (a *app) anCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <file...>",
		Short: "Check records offline (a service's stdout saved to a file): format, links, installs, sessions",
		Long: `Reads analytics records from files (other lines are skipped), validates each against the
record format, and runs them through the same indexer and rollups as the server, on a
temporary state. Nothing is sent anywhere. Exit code 1 when a record is invalid.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := analytics.CheckFiles(args)
			if err != nil {
				return err
			}
			if a.json() {
				if err := a.printJSON(rep); err != nil {
					return err
				}
			} else {
				a.printCheck(rep)
			}
			if rep.Invalid > 0 {
				return exitError(1)
			}
			return nil
		},
	}
}

func (a *app) printCheck(rep *analytics.CheckReport) {
	fmt.Fprintf(a.out, "%d lines: %d records (%d invalid, %d with warnings, %d duplicates), %d other lines\n",
		rep.Lines, rep.Records, rep.Invalid, rep.Warnings, rep.Dup, rep.Other)
	for _, p := range rep.Problems {
		fmt.Fprintf(a.out, "  %s\n", p)
	}
	fmt.Fprintf(a.out, "actors %d, persons %d (%d excluded as test), devices linked %d, sessions %d\n",
		rep.Actors, rep.Persons, rep.Excluded, rep.Merged, rep.Sessions)
	fmt.Fprintf(a.out, "installs %d, reinstalls %d, web first visits %d, suspicious %d\n",
		rep.Installs[analytics.KindInstall], rep.Installs[analytics.KindReinstall], rep.Installs[analytics.KindFirstVisit], rep.Installs["suspect"])
	if len(rep.Events) > 0 {
		w := a.table("EVENT", "COUNT", "ACTORS")
		names := make([]string, 0, len(rep.Events))
		for n := range rep.Events {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(w, "%s\t%d\t%d\n", n, rep.Events[n].N, rep.Events[n].Actors)
		}
		w.Flush()
	}
}

func (a *app) anSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Secrets of the archive and the Telegram digest, kept by clowdrd (never in deploy.yml)",
	}
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a secret from stdin (empty input removes it): " + strings.Join(analytics.SecretNames, ", "),
		Example: `  printf %s "$R2_KEY" | clowctl analytics secret set s3-key
  openssl rand -hex 8 | clowctl analytics secret set s3-prefix`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(analytics.SecretNames, args[0]) {
				return fmt.Errorf("unknown secret %q: %s", args[0], strings.Join(analytics.SecretNames, ", "))
			}
			b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
			if err != nil {
				return err
			}
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var res map[string][]string
			if err := t.client.Do(cmd.Context(), "PUT", api.Path("projects", t.project, "analytics", "secrets", args[0]),
				api.AnalyticsSecretRequest{Value: strings.TrimSpace(string(b))}, &res); err != nil {
				return err
			}
			fmt.Fprintf(a.err, "✓ %s; set: %s\n", args[0], strings.Join(res["secrets"], ", "))
			return nil
		},
	}
	cmd.AddCommand(set)
	return cmd
}

func (a *app) anRebuildCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the index and the rollups from the raw stream and the archive (admin token)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if since == "" {
				return errors.New("--since is required, e.g. --since 2026-01-01")
			}
			t, err := a.target(true)
			if err != nil {
				return err
			}
			ev, err := t.client.Stream(cmd.Context(), "POST", api.Path("projects", t.project, "analytics", "rebuild")+"?since="+url.QueryEscape(since), nil, a.progress)
			if err != nil {
				return err
			}
			if ev.Error != "" {
				return errors.New(ev.Error)
			}
			fmt.Fprintln(a.err, "✓ rebuilt")
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "first day to rebuild (records before it are kept as they are)")
	return cmd
}

func (a *app) anImportCmd() *cobra.Command {
	var dryRun, allowInvalid, liveDays bool
	cmd := &cobra.Command{
		Use:   "import <file>...",
		Short: "Bring records from elsewhere into the archive and rebuild from the first day they touch (admin token)",
		Long: `Takes analytics records kept before the project counted with clowdr (NDJSON, or NDJSON
compressed with zstd: .ndjson.zst) and writes each into the archived raw file of its event
day, merged with what is there and deduplicated by id; then the index and the rollups are
rebuilt from the first day it touched. Importing the same file again changes nothing.

The files are checked here first, as by "clowctl analytics check"; a file with invalid
records is refused unless --allow-invalid (they are left out). Days the project's stream
already covers are left out and listed; with --include-live-days they go into the stream
as late records received now (deduplicated by id against it, their days recomputed) — only
days within the late window (the last 8 days), so a tail that keeps arriving elsewhere is
imported at least weekly. Days before the stream need the archive (analytics.archive and its
secrets). Records of history should say src "import".`,
		Example: `  clowctl analytics import --dry-run history.ndjson.zst
  clowctl analytics import history.ndjson.zst -e prod
  clowctl analytics import --include-live-days tail.ndjson.zst -e prod`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := analytics.CheckFiles(args)
			if err != nil {
				return err
			}
			a.printCheck(rep)
			if rep.Invalid > 0 && !allowInvalid {
				return fmt.Errorf("%d invalid records: fix the file, or --allow-invalid to leave them out", rep.Invalid)
			}
			if dryRun {
				return nil
			}
			t, err := a.target(true)
			if err != nil {
				return err
			}
			for _, path := range args {
				body, err := importBody(path)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.err, "→ %s (%d KB)\n", path, len(body)>>10)
				path := api.Path("projects", t.project, "analytics", "import")
				if liveDays {
					path += "?live_days=1"
				}
				ev, err := t.client.Stream(cmd.Context(), "POST", path,
					api.RawBody{R: bytes.NewReader(body), Type: "application/zstd"}, a.progress)
				if err != nil {
					return err
				}
				if ev.Error != "" {
					return errors.New(ev.Error)
				}
			}
			fmt.Fprintln(a.err, "✓ imported")
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only check the files")
	cmd.Flags().BoolVar(&allowInvalid, "allow-invalid", false, "send files with invalid records (the server leaves them out)")
	cmd.Flags().BoolVar(&liveDays, "include-live-days", false, "take records of days the stream covers too, as late records (the last 8 days only)")
	return cmd
}

// importBody reads a file of records as zstd: as it is when compressed already.
func importBody(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(b, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return b, nil
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil), nil
}

// ─── Printing ────────────────────────────────────────────────────────────────

func (a *app) printAnalytics(res *analytics.Result) error {
	switch {
	case len(res.Series) > 0:
		a.printSeries(res.Series)
	case len(res.Funnels) > 0:
		a.printFunnels(res.Funnels)
	case len(res.Cohorts) > 0:
		a.printCohorts(res.Cohorts)
	case res.Person != nil:
		a.printPerson(res.Person)
	case len(res.Events) > 0:
		for _, e := range res.Events {
			fmt.Fprintln(a.out, string(e))
		}
	case len(res.Rows) > 0 || len(res.Screens) > 0:
		if len(res.Rows) > 0 {
			a.printRows(res.Columns, res.Rows)
		}
		if len(res.Screens) > 0 {
			if len(res.Rows) > 0 {
				fmt.Fprintln(a.out)
			}
			a.printRows(res.ScreenColumns, res.Screens)
		}
	default:
		fmt.Fprintln(a.out, "(nothing)")
	}
	var notes []string
	if res.ExcludedTest > 0 {
		notes = append(notes, fmt.Sprintf("%d test persons left out (--include-test)", res.ExcludedTest))
	}
	if res.Note != "" {
		notes = append(notes, res.Note)
	}
	if len(res.Archive) > 0 {
		notes = append(notes, fmt.Sprintf("%d archive files: clowctl analytics archive", len(res.Archive)))
	}
	for _, n := range notes {
		fmt.Fprintln(a.err, "note: "+n)
	}
	return nil
}

func (a *app) printSeries(series []analytics.Series) {
	headers := []string{"TIME"}
	for _, s := range series {
		h := s.Event
		if s.Breakdown != "" {
			h = s.Breakdown
			if len(series) > 1 && series[0].Event != series[len(series)-1].Event {
				h = s.Event + "/" + s.Breakdown
			}
		}
		headers = append(headers, h)
		if len(s.Previous) > 0 {
			headers = append(headers, "PREV")
		}
	}
	w := a.table(headers...)
	for i, p := range series[0].Points {
		cells := []string{p.T}
		for _, s := range series {
			v := "-"
			if i < len(s.Points) {
				v = num(s.Points[i].V)
			}
			cells = append(cells, v)
			if len(s.Previous) > 0 {
				pv := "-"
				if i < len(s.Previous) {
					pv = num(s.Previous[i].V)
				}
				cells = append(cells, pv)
			}
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	// dau, wau and mau do not add up over a range: the server sends their daily average.
	label := "TOTAL"
	if series[0].TotalKind == "average" {
		label = "AVG"
	}
	totals := []string{label}
	for _, s := range series {
		totals = append(totals, num(s.Total))
		if len(s.Previous) > 0 {
			totals = append(totals, "")
		}
	}
	fmt.Fprintln(w, strings.Join(totals, "\t"))
	w.Flush()
}

func pct(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}

func (a *app) printFunnels(fs []analytics.Funnel) {
	for i, f := range fs {
		if f.Breakdown != "" {
			if i > 0 {
				fmt.Fprintln(a.out)
			}
			fmt.Fprintf(a.out, "%s:\n", f.Breakdown)
		}
		w := a.table("STEP", "ACTORS", "FROM PREV", "FROM START", "MEDIAN TIME")
		for _, s := range f.Steps {
			med := "-"
			if s.MedianSeconds != nil {
				med = humanSeconds(*s.MedianSeconds)
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", s.Step, s.Count, pct(s.FromPrev), pct(s.FromStart), med)
		}
		w.Flush()
	}
}

func humanSeconds(s float64) string {
	switch {
	case s >= 86400:
		return fmt.Sprintf("%.1fd", s/86400)
	case s >= 3600:
		return fmt.Sprintf("%.1fh", s/3600)
	case s >= 60:
		return fmt.Sprintf("%.0fm", s/60)
	}
	return fmt.Sprintf("%.0fs", s)
}

func (a *app) printCohorts(cs []analytics.Cohort) {
	n := 0
	br := false
	for _, c := range cs {
		n = max(n, len(c.Values))
		br = br || c.Breakdown != ""
	}
	headers := []string{"COHORT"}
	if br {
		headers = append(headers, "BREAKDOWN")
	}
	headers = append(headers, "SIZE")
	for i := 0; i < n; i++ {
		headers = append(headers, fmt.Sprintf("+%d", i))
	}
	w := a.table(headers...)
	for _, c := range cs {
		cells := []string{c.Start}
		if br {
			cells = append(cells, dash(c.Breakdown))
		}
		cells = append(cells, fmt.Sprint(c.Size))
		for i := 0; i < n; i++ {
			switch {
			case i >= len(c.Values):
				cells = append(cells, "")
			case c.Size == 0:
				cells = append(cells, "-")
			default:
				cells = append(cells, fmt.Sprintf("%.0f%%", 100*float64(c.Values[i])/float64(c.Size)))
			}
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()
}

func (a *app) printPerson(p *analytics.PersonResult) {
	fmt.Fprintf(a.out, "user %s, first seen %s, last %s\n", p.User, dash(p.FirstTS), dash(p.LastTS))
	if p.Actor != nil {
		fmt.Fprintf(a.out, "first: %s %s %s, source %s %s\n", dash(p.Actor.FirstDay), dash(p.Actor.FirstApp), dash(p.Actor.FirstPF), dash(p.Actor.FirstSource), p.Actor.FirstCampaign)
	}
	if len(p.Devices) > 0 {
		fmt.Fprintf(a.out, "devices: %s\n", strings.Join(p.Devices, ", "))
	}
	keys := make([]string, 0, len(p.Props))
	for k := range p.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(a.out, "  %s = %v\n", k, p.Props[k])
	}
	for _, e := range p.Events {
		fmt.Fprintln(a.out, string(e))
	}
}

func (a *app) printRows(cols []string, rows []analytics.Row) {
	if len(cols) == 0 {
		seen := map[string]bool{}
		for _, r := range rows {
			for k := range r {
				if !seen[k] {
					seen[k] = true
					cols = append(cols, k)
				}
			}
		}
		sort.Strings(cols)
	}
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = strings.ToUpper(c)
	}
	w := a.table(headers...)
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = cell(r[c])
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case float64:
		return num(x)
	case string:
		return dash(x)
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = fmt.Sprint(p)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

// ─── apply and diff ──────────────────────────────────────────────────────────

// analyticsSync compares the `analytics:` block of deploy.yml with the server's. supported is
// false when the server does not know analytics.
func analyticsSync(ctx context.Context, c *api.Client, project string, local *config.AnalyticsConf) (remote *config.AnalyticsConf, same, supported bool, err error) {
	var v api.Version
	if err := c.Do(ctx, "GET", "/v1/version", nil, &v); err != nil {
		return nil, false, false, err
	}
	if !slices.Contains(v.Features, api.FeatureAnalytics) {
		return nil, local == nil, false, nil
	}
	if err := c.Do(ctx, "GET", api.Path("projects", project, "analytics", "config"), nil, &remote); err != nil {
		return nil, false, true, err
	}
	return remote, confJSON(local) == confJSON(remote), true, nil
}

func confJSON(c *config.AnalyticsConf) string {
	if c == nil {
		return "none"
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// applyAnalytics sends the `analytics:` block of deploy.yml to the server.
func (a *app) applyAnalytics(ctx context.Context, t *target, d *config.Deploy) error {
	_, same, supported, err := analyticsSync(ctx, t.client, t.project, d.Analytics)
	switch {
	case err != nil:
		return err
	case !supported && d.Analytics != nil:
		return errors.New("deploy.yml has analytics, but the server does not support it: update clowdrd")
	case !supported || same:
		return nil
	}
	path := api.Path("projects", t.project, "analytics", "config")
	if d.Analytics == nil {
		err = t.client.Do(ctx, "DELETE", path, nil, nil)
	} else {
		err = t.client.Do(ctx, "PUT", path, d.Analytics, nil)
	}
	if err == nil {
		fmt.Fprintln(a.err, "✓ analytics config applied")
	}
	return err
}
