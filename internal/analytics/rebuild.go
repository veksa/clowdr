package analytics

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/veksa/clowdr/internal/logs"
)

// Rebuild indexes every raw day received since `since` again, then recomputes the rollups of
// the event days from `since` on. A day comes from the stream while it is in the hot window,
// from the archive before that, so this is also how a lost disk is restored and how imported
// history (in the archive layout) gets in. Config, secrets and schedules stay.
//
// The index is thrown away and built from scratch only when `since` reaches back to the first
// day the project knows (its first rollup, raw day or archived day): rollups are kept forever
// and their bitmaps hold actor and group numbers, so renumbering under rollups older than
// `since` would silently mix up their actors, and persons, device links and install keys seen
// before `since` would be lost. A later `since` keeps the index and applies the records on top
// of it (indexing is idempotent).
func (p *Project) Rebuild(ctx context.Context, since time.Time, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	since = since.UTC().Truncate(24 * time.Hour)
	c, base, s3err := p.S3()
	windowFrom := p.WindowStart()
	windowStart := windowFrom.Truncate(24 * time.Hour)
	if since.Before(windowStart) && s3err != nil {
		return fmt.Errorf("days before %s are only in the archive: %v", windowStart.Format(DayLayout), s3err)
	}

	p.rollupMu.Lock()
	defer p.rollupMu.Unlock()
	p.indexMu.Lock()
	defer p.indexMu.Unlock()
	started := p.m.now()

	full := !since.After(dayTime(p.firstKnownDay()))
	if full {
		progress("from the first day: the index is built from scratch")
	} else {
		progress("days before " + since.Format(DayLayout) + " stay: the records are applied to the index as it is")
	}
	if err := p.state.Update(func(t *Tx) error {
		if !full {
			return nil
		}
		for _, b := range [][]byte{bActors, bActorIDs, bPersons, bLinks, bInstall, bGroups} {
			if err := t.tx.DeleteBucket(b); err != nil {
				return err
			}
			if _, err := t.tx.CreateBucket(b); err != nil {
				return err
			}
		}
		return t.tx.Bucket(bMeta).Delete([]byte("catalog"))
	}); err != nil {
		return err
	}

	exclude := map[string]any{"test": true}
	if cfg := p.Config(); cfg != nil {
		exclude = cfg.ExcludeRule()
	}
	byDay := map[string][]Record{}
	done := map[string]bool{}
	var changed []string
	finalize := func(day string) error {
		recs := byDay[day]
		delete(byDay, day)
		done[day] = true
		if len(recs) == 0 {
			if old, _ := LoadRollup(p.dir, day); old == nil {
				return nil
			}
		}
		roll, err := ComputeDay(p.state, day, recs, exclude, p.m.now())
		if err != nil {
			return err
		}
		if ok, err := SaveRollup(p.dir, roll); err != nil {
			return err
		} else if ok {
			changed = append(changed, day)
		}
		return nil
	}
	sinceDay := since.Format(DayLayout)
	today := started.Truncate(24 * time.Hour)
	for d := since; !d.After(today); d = d.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return err
		}
		day := d.Format(DayLayout)
		var lines []string
		from := "stream"
		inStream := !d.Before(windowStart)
		// The day the hot window starts in has lost its first hours to retention: the archive
		// holds it whole, so it is asked first.
		archiveFirst := inStream && d.Before(windowFrom) && s3err == nil
		if inStream && !archiveFirst {
			var err error
			if lines, err = p.rawLines(day, -1); err != nil {
				return err
			}
		}
		if len(lines) == 0 && s3err == nil {
			ls, parts, err := p.fetchRawDay(ctx, c, base, day)
			if err != nil {
				return err
			}
			if parts > 0 {
				lines, from = ls, "archive"
				a := p.archiveState()
				a.Raw[day] = parts
				p.state.SetMeta("archive", a)
			}
		}
		if len(lines) == 0 && archiveFirst {
			var err error
			if lines, err = p.rawLines(day, -1); err != nil {
				return err
			}
		}
		recs := make([]Record, 0, len(lines))
		for _, l := range lines {
			if r, err := Parse(l); err == nil {
				recs = append(recs, r)
			}
		}
		if _, err := Index(p.state, recs); err != nil {
			return err
		}
		for _, r := range recs {
			if ed := r.Day(); ed >= sinceDay {
				byDay[ed] = append(byDay[ed], r)
			}
		}
		if len(recs) > 0 {
			progress(fmt.Sprintf("%s: %d records (%s)", day, len(recs), from))
		}
		// An event day is complete once late events can no longer arrive for it.
		cut := d.AddDate(0, 0, -lateDays).Format(DayLayout)
		var ready []string
		for ed := range byDay {
			if ed <= cut {
				ready = append(ready, ed)
			}
		}
		sort.Strings(ready)
		for _, ed := range ready {
			if err := finalize(ed); err != nil {
				return err
			}
		}
	}
	// What is left, and the days in range that had a rollup but no records now.
	left := map[string]bool{}
	for ed := range byDay {
		left[ed] = true
	}
	for _, ed := range RollupDays(p.dir) {
		if ed >= sinceDay && !done[ed] {
			left[ed] = true
		}
	}
	days := make([]string, 0, len(left))
	for ed := range left {
		days = append(days, ed)
	}
	sort.Strings(days)
	for _, ed := range days {
		if err := finalize(ed); err != nil {
			return err
		}
	}

	// Lines that arrived while the rebuild ran may have missed the paused indexer.
	recent, err := p.readRecords(started.Add(-time.Minute), time.Time{}, nil)
	if err != nil {
		return err
	}
	if _, err := Index(p.state, recent); err != nil {
		return err
	}
	if len(changed) > 0 {
		var pending []string
		p.state.GetMeta("rollups_pending_upload", &pending)
		p.state.SetMeta("rollups_pending_upload", mergeDays(pending, changed))
	}
	progress(fmt.Sprintf("done: %d rollups changed", len(changed)))
	return nil
}

// firstKnownDay is the earliest day the project has anything for: a rollup, a raw record in the
// hot window or a raw day in the archive state; "9999-12-31" when it has nothing.
func (p *Project) firstKnownDay() string {
	first := "9999-12-31"
	if ds := RollupDays(p.dir); len(ds) > 0 {
		first = ds[0]
	}
	if p.m.Logs != nil {
		for _, svc := range p.m.Logs.Streams(p.name, logs.KindAnalytics) {
			if o := p.m.Logs.Oldest(logs.Key{Project: p.name, Kind: logs.KindAnalytics, Name: svc}); !o.IsZero() {
				first = min(first, o.UTC().Format(DayLayout))
			}
		}
	}
	for d, parts := range p.archiveState().Raw {
		if parts > 0 {
			first = min(first, d)
		}
	}
	return first
}
