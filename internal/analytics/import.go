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
	// Skipped are the days at or after the first day of the stream, left out with their records:
	// clowdr archives those days from what it received, and an imported day would hide them.
	Skipped        []string `json:"skipped,omitempty"`
	SkippedRecords int      `json:"skipped_records,omitempty"`
}

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

// Import brings records from elsewhere (history kept before the project counted with clowdr) into
// the archive: each record goes to the raw file of its event day, merged with what that file holds
// and deduplicated by id, then the index and the rollups are rebuilt from the first day it touched.
// Invalid lines are left out and reported, as are days the stream already covers. Importing the
// same records again changes nothing. It needs the archive (analytics.archive and its secrets).
func (p *Project) Import(ctx context.Context, lines []string, progress func(string)) (*ImportReport, error) {
	if progress == nil {
		progress = func(string) {}
	}
	c, base, err := p.S3()
	if err != nil {
		return nil, fmt.Errorf("an import goes to the archive: %v", err)
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
	days := make([]string, 0, len(byDay))
	for d, ls := range byDay {
		if d >= cut {
			rep.Skipped = append(rep.Skipped, d)
			rep.SkippedRecords += len(ls)
			continue
		}
		days = append(days, d)
	}
	sort.Strings(days)
	sort.Strings(rep.Skipped)
	if len(rep.Skipped) > 0 {
		progress(fmt.Sprintf("%d records of %s…%s left out: the stream covers days from %s",
			rep.SkippedRecords, rep.Skipped[0], rep.Skipped[len(rep.Skipped)-1], cut))
	}

	var changed []string
	p.archMu.Lock()
	for _, day := range days {
		if err := ctx.Err(); err != nil {
			p.archMu.Unlock()
			return rep, err
		}
		n, present, err := p.importDay(ctx, c, base, day, byDay[day])
		if err != nil {
			p.archMu.Unlock()
			return rep, fmt.Errorf("%s: %w", day, err)
		}
		rep.Imported += n
		rep.Present += present
		if n > 0 {
			changed = append(changed, day)
			progress(fmt.Sprintf("%s: %d records imported, %d there already", day, n, present))
		}
	}
	p.archMu.Unlock()
	rep.Days = changed
	if len(changed) == 0 {
		progress("nothing new: every record is in the archive already")
		return rep, nil
	}
	if err := p.Rebuild(ctx, dayTime(changed[0]), progress); err != nil {
		return rep, err
	}
	return rep, nil
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
