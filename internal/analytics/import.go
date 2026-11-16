package analytics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/veksa/clowdr/internal/logs"
)

// ImportReport is what an import did.
type ImportReport struct {
	Lines    int      `json:"lines"`
	Records  int      `json:"records"`
	Invalid  int      `json:"invalid"`  // lines that are no record clowdr can count, left out
	Warnings int      `json:"warnings"` // records kept with format problems
	Problems []string `json:"problems,omitempty"`
	Imported int      `json:"imported"` // records written to the archive
	Present  int      `json:"present"`  // records the archive (or the file) had already, by id
	Days     []string `json:"days,omitempty"`
	// Skipped are the days at or after the first day of the stream, left out with their records
	// unless live days are asked for: clowdr archives those days from what it received, and an
	// imported day file would hide them.
	Skipped        []string `json:"skipped,omitempty"`
	SkippedRecords int      `json:"skipped_records,omitempty"`
	// Live are the days of the stream the records went into, as late records received now.
	Live []string `json:"live,omitempty"`
	// TooLate are days of the stream older than the late window: a record may arrive at most
	// lateDays after its day, or a later rebuild would count that day from the late records alone.
	TooLate        []string `json:"too_late,omitempty"`
	TooLateRecords int      `json:"too_late_records,omitempty"`
}

// ImportOptions change what an import does.
type ImportOptions struct {
	// LiveDays takes records of days the stream already covers too: they go into the stream as
	// late records received now (deduplicated by id against what it holds), and those days'
	// rollups are recomputed. Only days within the late window (lateDays) qualify.
	LiveDays bool
}

// importService is the stream records of live days are written to.
const importService = "import"

// maxImportBytes bounds the uncompressed records of one import.
const maxImportBytes = 4 << 30

// zstdMagic starts every zstd frame.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// RecordsReader returns r uncompressed: zstd when it starts with the zstd magic, as is otherwise.
func RecordsReader(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(len(zstdMagic))
	if !bytes.Equal(head, zstdMagic) {
		return io.NopCloser(br), nil
	}
	d, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(1<<27))
	if err != nil {
		return nil, err
	}
	return d.IOReadCloser(), nil
}

// ReadImport reads the lines of an import body (NDJSON, or NDJSON+zstd).
func ReadImport(r io.Reader) ([]string, error) {
	rc, err := RecordsReader(r)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var lines []string
	read := 0
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		read += len(sc.Bytes()) + 1
		if read > maxImportBytes {
			return nil, fmt.Errorf("more than %d GB of records: split the file", maxImportBytes>>30)
		}
		if l := sc.Text(); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines, sc.Err()
}

// ImportCutoff is the first day records can no longer be imported into: the day the project's
// stream begins, or today when it has received nothing yet.
func (p *Project) ImportCutoff() string { return p.streamFrom().UTC().Format(DayLayout) }

type importLine struct {
	id   string
	t    time.Time
	line string
}

// Import brings records from elsewhere (history kept before the project counted with clowdr) in.
// A record of a day before the stream goes to the archived raw file of its event day, merged with
// what that file holds and deduplicated by id, and the index and the rollups are rebuilt from the
// first such day; that needs the archive (analytics.archive and its secrets). A record of a day
// the stream covers is left out, or with opts.LiveDays goes into the stream as a late record (see
// ImportOptions). Invalid lines are left out and reported. Importing the same records again
// changes nothing.
func (p *Project) Import(ctx context.Context, lines []string, opts ImportOptions, progress func(string)) (*ImportReport, error) {
	if progress == nil {
		progress = func(string) {}
	}
	rep := &ImportReport{}
	byDay := map[string][]importLine{}
	for n, l := range lines {
		rep.Lines++
		i := strings.Index(l, Mark)
		if i < 0 {
			rep.Invalid++
			if len(rep.Problems) < maxProblems {
				rep.Problems = append(rep.Problems, fmt.Sprintf("line %d: not an analytics record", n+1))
			}
			continue
		}
		rep.Records++
		r, err := Parse(l[i:])
		if err != nil {
			rep.Invalid++
			if len(rep.Problems) < maxProblems {
				rep.Problems = append(rep.Problems, fmt.Sprintf("line %d: %v", n+1, err))
			}
			continue
		}
		if ps := Validate(&r); len(ps) > 0 {
			rep.Warnings++
			if len(rep.Problems) < maxProblems {
				rep.Problems = append(rep.Problems, fmt.Sprintf("line %d: %s", n+1, strings.Join(ps, "; ")))
			}
		}
		byDay[r.Day()] = append(byDay[r.Day()], importLine{id: r.ID, t: r.Time(), line: l[i:]})
	}
	cut := p.ImportCutoff()
	// The oldest day a record received now still counts in (see lateDays).
	late := p.m.now().Truncate(24*time.Hour).AddDate(0, 0, -(lateDays - 1)).Format(DayLayout)
	days := make([]string, 0, len(byDay))
	var live []string
	for d, ls := range byDay {
		switch {
		case d < cut:
			days = append(days, d)
		case !opts.LiveDays:
			rep.Skipped = append(rep.Skipped, d)
			rep.SkippedRecords += len(ls)
		case d < late:
			rep.TooLate = append(rep.TooLate, d)
			rep.TooLateRecords += len(ls)
		default:
			live = append(live, d)
		}
	}
	sort.Strings(days)
	sort.Strings(live)
	sort.Strings(rep.Skipped)
	sort.Strings(rep.TooLate)
	if len(rep.Skipped) > 0 {
		progress(fmt.Sprintf("%d records of %s…%s left out: the stream covers days from %s (live days were not asked for)",
			rep.SkippedRecords, rep.Skipped[0], rep.Skipped[len(rep.Skipped)-1], cut))
	}
	if len(rep.TooLate) > 0 {
		progress(fmt.Sprintf("%d records of %s…%s left out: older than the late window (days from %s take records now)",
			rep.TooLateRecords, rep.TooLate[0], rep.TooLate[len(rep.TooLate)-1], late))
	}

	var changed []string
	if len(days) > 0 {
		if err := p.importArchived(ctx, days, byDay, rep, progress, &changed); err != nil {
			return rep, err
		}
	}
	if len(live) > 0 {
		if err := p.importLive(live, byDay, rep, progress); err != nil {
			return rep, err
		}
	}
	rep.Days = changed
	if len(changed) == 0 && len(rep.Live) == 0 {
		progress("nothing new: every record is there already")
		return rep, nil
	}
	if len(changed) > 0 {
		if err := p.Rebuild(ctx, dayTime(changed[0]), progress); err != nil {
			return rep, err
		}
	} else if _, err := p.Recompute(rep.Live); err != nil {
		return rep, err
	}
	return rep, nil
}

// importArchived merges the records of days before the stream into their archived raw files.
func (p *Project) importArchived(ctx context.Context, days []string, byDay map[string][]importLine, rep *ImportReport, progress func(string), changed *[]string) error {
	c, base, err := p.S3()
	if err != nil {
		return fmt.Errorf("records of days before the stream go to the archive: %v", err)
	}
	p.archMu.Lock()
	for _, day := range days {
		if err := ctx.Err(); err != nil {
			p.archMu.Unlock()
			return err
		}
		n, present, err := p.importDay(ctx, c, base, day, byDay[day])
		if err != nil {
			p.archMu.Unlock()
			return fmt.Errorf("%s: %w", day, err)
		}
		rep.Imported += n
		rep.Present += present
		if n > 0 {
			*changed = append(*changed, day)
			progress(fmt.Sprintf("%s: %d records imported, %d there already", day, n, present))
		}
	}
	p.archMu.Unlock()
	return nil
}

// importLive writes the records of days the stream covers into it as late records received now,
// leaving out ids the stream holds for those days, and waits for the indexer. The caller
// recomputes the days (a rebuild that follows does it too).
func (p *Project) importLive(days []string, byDay map[string][]importLine, rep *ImportReport, progress func(string)) error {
	if p.m.Logs == nil {
		return fmt.Errorf("this server keeps no stream")
	}
	want := map[string]bool{}
	for _, d := range days {
		want[d] = true
	}
	seen := map[string]bool{}
	// The whole window: a record's receive time says nothing certain about its day.
	if err := p.ReadRaw(p.streamFrom(), time.Time{}, func(_ time.Time, line string) {
		if r, err := Parse(line); err == nil && want[r.Day()] {
			seen[r.ID] = true
		}
	}); err != nil {
		return err
	}
	k := logs.Key{Project: p.name, Kind: logs.KindAnalytics, Name: importService}
	now := p.m.now()
	for _, d := range days {
		n, present := 0, 0
		for _, r := range byDay[d] {
			if seen[r.id] {
				present++
				continue
			}
			seen[r.id] = true
			l := logs.Line{T: now.UnixNano(), L: r.line}
			if err := p.m.Logs.Append(k, l); err != nil {
				return err
			}
			p.m.OnLine(k, l)
			n++
		}
		rep.Imported += n
		rep.Present += present
		if n > 0 {
			rep.Live = append(rep.Live, d)
			progress(fmt.Sprintf("%s: %d records into the stream, %d there already", d, n, present))
		}
	}
	p.Sync()
	return nil
}

// importDay merges records into the archived raw file of a day and reports how many were new and
// how many it had already.
func (p *Project) importDay(ctx context.Context, c *S3, base, day string, recs []importLine) (int, int, error) {
	old, _, err := p.fetchRawDay(ctx, c, base, day)
	if err != nil {
		return 0, 0, err
	}
	merged := make([]importLine, 0, len(old)+len(recs))
	seen := make(map[string]bool, len(old)+len(recs))
	for _, l := range old {
		il := importLine{line: l}
		if r, err := Parse(l); err == nil {
			il.id, il.t = r.ID, r.Time()
			seen[r.ID] = true
		}
		merged = append(merged, il)
	}
	n, present := 0, 0
	for _, r := range recs {
		if seen[r.id] {
			present++
			continue
		}
		seen[r.id] = true
		merged = append(merged, r)
		n++
	}
	if n == 0 {
		return 0, present, nil
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].t.Before(merged[j].t) })
	out := make([]string, len(merged))
	for i, l := range merged {
		out[i] = l.line
	}
	body := compressBest(joinLines(out))
	if len(body) > maxObject {
		return 0, 0, fmt.Errorf("the day would be %d MB compressed, more than one archive object holds", len(body)>>20)
	}
	if err := c.Put(ctx, RawKey(base, day, -1), body, "application/zstd"); err != nil {
		return 0, 0, err
	}
	a := p.archiveState()
	a.Raw[day] = 1
	p.state.SetMeta("archive", a)
	return n, present, nil
}
