package analytics

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ArchiveState is what has been uploaded, kept in meta.
type ArchiveState struct {
	// Raw maps a day (by receive time) to the number of parts uploaded: 1 for a day file, 24
	// for a day split by hour, 0 for a day without records (nothing uploaded).
	Raw         map[string]int    `json:"raw,omitempty"`
	State       map[string]string `json:"state,omitempty"` // snapshot day → uploaded at
	LastOK      string            `json:"last_ok,omitempty"`
	LastError   string            `json:"last_error,omitempty"`
	LastErrorAt string            `json:"last_error_at,omitempty"`
}

// maxObject is the largest object the archive writes in one PUT; a larger day is split by hour.
const maxObject = 100 << 20

// archiveRetryDays bounds how many missing days one run uploads.
const archiveRetryDays = 40

func (p *Project) archiveState() ArchiveState {
	var a ArchiveState
	p.state.GetMeta("archive", &a)
	if a.Raw == nil {
		a.Raw = map[string]int{}
	}
	if a.State == nil {
		a.State = map[string]string{}
	}
	return a
}

// S3 returns the archive client and key prefix, or an error saying what is missing.
func (p *Project) S3() (*S3, string, error) {
	cfg := p.Config()
	if cfg == nil || !cfg.ArchiveOn() {
		return nil, "", errors.New("no archive configured (analytics.archive in deploy.yml)")
	}
	s := p.secrets()
	var missing []string
	for _, n := range []string{SecretS3Key, SecretS3Secret, SecretS3Prefix} {
		if s[n] == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, "", fmt.Errorf("archive secrets not set: %s (clowctl analytics secret set)", strings.Join(missing, ", "))
	}
	a := cfg.Archive
	c := &S3{Endpoint: a.Endpoint, Region: a.Region, Bucket: a.Bucket, AccessKey: s[SecretS3Key], SecretKey: s[SecretS3Secret],
		Virtual: a.Style == "virtual", HTTP: p.m.httpClient()}
	return c, s[SecretS3Prefix] + "/v1/" + p.name, nil
}

// RawKey is the archive key of a raw day; hour >= 0 names one part of a split day.
func RawKey(base, day string, hour int) string {
	k := base + "/raw/" + day[:4] + "/" + day[5:7] + "/" + day
	if hour >= 0 {
		k += fmt.Sprintf("-%02d", hour)
	}
	return k + ".ndjson.zst"
}

// RollupKey is the archive key of a day's rollup.
func RollupKey(base, day string) string { return base + "/rollup/" + day[:4] + "/" + day + ".roll.zst" }

// StateKey is the archive key of a weekly state snapshot.
func StateKey(base, day string) string { return base + "/state/" + day + ".ndjson.zst" }

func (p *Project) recordArchive(err error) {
	a := p.archiveState()
	now := p.m.now().Format(time.RFC3339)
	if err != nil {
		a.LastError, a.LastErrorAt = err.Error(), now
	} else {
		a.LastOK = now
	}
	p.state.SetMeta("archive", a)
}

// ArchiveDue uploads every complete day of the hot window not uploaded yet (by receive time, so
// a late event never changes an uploaded day), the rollups that changed, and on Sundays a
// snapshot of the state.
func (p *Project) ArchiveDue(ctx context.Context) error {
	c, base, err := p.S3()
	if err != nil {
		return err
	}
	p.archMu.Lock()
	defer p.archMu.Unlock()
	now := p.m.now()
	a := p.archiveState()
	start := p.WindowStart().Truncate(24 * time.Hour)
	var days []string
	for d := start; ; d = d.AddDate(0, 0, 1) {
		if now.Before(d.AddDate(0, 0, 1).Add(90 * time.Minute)) {
			break // the day is not over (with time for the last flush)
		}
		day := d.Format(DayLayout)
		if _, done := a.Raw[day]; !done {
			days = append(days, day)
		}
	}
	if len(days) > archiveRetryDays {
		days = days[len(days)-archiveRetryDays:]
	}
	var firstErr error
	for _, day := range days {
		if err := ctx.Err(); err != nil {
			return err
		}
		parts, err := p.uploadRawDay(ctx, c, base, day)
		if err != nil {
			firstErr = err
			break
		}
		a = p.archiveState()
		a.Raw[day] = parts
		p.state.SetMeta("archive", a)
	}
	if firstErr == nil {
		firstErr = p.uploadRollups(ctx, c, base)
	}
	if firstErr == nil && now.Weekday() == time.Sunday {
		day := now.Format(DayLayout)
		if _, done := a.State[day]; !done {
			var buf bytes.Buffer
			if err := p.state.WriteSnapshot(&buf); err != nil {
				firstErr = err
			} else if err := c.Put(ctx, StateKey(base, day), compressBest(buf.Bytes()), "application/zstd"); err != nil {
				firstErr = err
			} else {
				a = p.archiveState()
				a.State[day] = now.Format(time.RFC3339)
				p.state.SetMeta("archive", a)
			}
		}
	}
	p.recordArchive(firstErr)
	return firstErr
}

// uploadRollups sends the rollups recomputed since the last upload.
func (p *Project) uploadRollups(ctx context.Context, c *S3, base string) error {
	var pending []string
	p.state.GetMeta("rollups_pending_upload", &pending)
	done := 0
	var err error
	for _, day := range pending {
		raw, rerr := os.ReadFile(rollupPath(p.dir, day))
		if rerr != nil {
			done++ // gone: nothing to send
			continue
		}
		if err = c.Put(ctx, RollupKey(base, day), raw, "application/zstd"); err != nil {
			break
		}
		done++
	}
	p.rollupMu.Lock()
	var now []string
	p.state.GetMeta("rollups_pending_upload", &now)
	sent := map[string]bool{}
	for _, d := range pending[:done] {
		sent[d] = true
	}
	var left []string
	for _, d := range now {
		if !sent[d] {
			left = append(left, d)
		}
	}
	p.state.SetMeta("rollups_pending_upload", left)
	p.rollupMu.Unlock()
	return err
}

// rawLines returns the raw records received during a day, in receive order.
func (p *Project) rawLines(day string, hour int) ([]string, error) {
	from, err := time.Parse(DayLayout, day)
	if err != nil {
		return nil, err
	}
	to := from.AddDate(0, 0, 1)
	if hour >= 0 {
		from = from.Add(time.Duration(hour) * time.Hour)
		to = from.Add(time.Hour)
	}
	type tl struct {
		t time.Time
		l string
	}
	var all []tl
	err = p.ReadRaw(from, to, func(t time.Time, l string) {
		if !t.Before(from) {
			all = append(all, tl{t, l})
		}
	})
	sort.SliceStable(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	out := make([]string, len(all))
	for i, x := range all {
		out[i] = x.l
	}
	return out, err
}

func joinLines(lines []string) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

var zbest, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))

func compressBest(b []byte) []byte {
	zmu.Lock()
	defer zmu.Unlock()
	return zbest.EncodeAll(b, nil)
}

// uploadRawDay writes one day as NDJSON+zstd (split by hour when larger than maxObject) and
// returns the number of parts.
func (p *Project) uploadRawDay(ctx context.Context, c *S3, base, day string) (int, error) {
	lines, err := p.rawLines(day, -1)
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		return 0, nil
	}
	body := compressBest(joinLines(lines))
	if len(body) <= maxObject {
		return 1, c.Put(ctx, RawKey(base, day, -1), body, "application/zstd")
	}
	for h := 0; h < 24; h++ {
		ls, err := p.rawLines(day, h)
		if err != nil {
			return 0, err
		}
		if err := c.Put(ctx, RawKey(base, day, h), compressBest(joinLines(ls)), "application/zstd"); err != nil {
			return 0, err
		}
	}
	return 24, nil
}

// ArchiveURLs returns presigned GET URLs (valid for ttl) of the raw days in [since, until]. Days
// the archive state does not know (an import, a lost state) are looked up with HEAD when probe
// is set; found ones are remembered.
func (p *Project) ArchiveURLs(ctx context.Context, since, until time.Time, ttl time.Duration, probe bool) ([]string, error) {
	c, base, err := p.S3()
	if err != nil {
		return nil, err
	}
	a := p.archiveState()
	var urls []string
	learned := false
	for d := since.UTC().Truncate(24 * time.Hour); !d.After(until); d = d.AddDate(0, 0, 1) {
		day := d.Format(DayLayout)
		parts, known := a.Raw[day]
		if !known && probe {
			if _, err := c.Head(ctx, RawKey(base, day, -1)); err == nil {
				parts, known = 1, true
			} else if _, err := c.Head(ctx, RawKey(base, day, 0)); err == nil {
				parts, known = 24, true
			}
			if known {
				a.Raw[day], learned = parts, true
			}
		}
		switch parts {
		case 1:
			u, err := c.Presign(RawKey(base, day, -1), ttl)
			if err != nil {
				return nil, err
			}
			urls = append(urls, u)
		case 24:
			for h := 0; h < 24; h++ {
				u, err := c.Presign(RawKey(base, day, h), ttl)
				if err != nil {
					return nil, err
				}
				urls = append(urls, u)
			}
		}
	}
	if learned {
		p.state.SetMeta("archive", a)
	}
	return urls, nil
}

// fetchRawDay reads a raw day back from the archive with the number of its parts (1, or 24 for
// a day split by hour); 0 parts when it is not there.
func (p *Project) fetchRawDay(ctx context.Context, c *S3, base, day string) ([]string, int, error) {
	var bodies [][]byte
	b, err := c.Get(ctx, RawKey(base, day, -1))
	switch {
	case err == nil:
		bodies = append(bodies, b)
	case errors.Is(err, ErrNotFound):
		for h := 0; h < 24; h++ {
			b, err := c.Get(ctx, RawKey(base, day, h))
			if errors.Is(err, ErrNotFound) {
				if h == 0 {
					return nil, 0, nil
				}
				continue
			} else if err != nil {
				return nil, 0, err
			}
			bodies = append(bodies, b)
		}
	default:
		return nil, 0, err
	}
	parts := 1
	if len(bodies) > 1 || errors.Is(err, ErrNotFound) {
		parts = 24
	}
	var lines []string
	for _, body := range bodies {
		raw, err := decompress(body)
		if err != nil {
			return nil, 0, fmt.Errorf("archive %s: %w", day, err)
		}
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			if l := sc.Text(); l != "" {
				lines = append(lines, l)
			}
		}
	}
	return lines, parts, nil
}

// Restore fills a new or lost index from the archive: the archived rollups the disk lacks, the
// latest state snapshot (actors and groups keep the numbers those rollups refer to) and the raw
// days received since that snapshot, applied on top. Without a snapshot it indexes every
// archived raw day from scratch and leaves the archived rollups alone (their numbers would not
// match). It reports whether there was anything to restore.
func (p *Project) Restore(ctx context.Context, progress func(string)) (bool, error) {
	if progress == nil {
		progress = func(string) {}
	}
	c, base, err := p.S3()
	if err != nil {
		return false, err
	}
	snaps, err := c.List(ctx, base+"/state/")
	if err != nil {
		return false, err
	}
	raws, err := c.List(ctx, base+"/raw/")
	if err != nil {
		return false, err
	}
	if len(snaps) == 0 && len(raws) == 0 {
		return false, nil
	}
	// The archive knows its raw days, so status and archive links do not probe for them.
	a := p.archiveState()
	for _, k := range raws {
		name := k[strings.LastIndex(k, "/")+1:]
		day := name[:min(len(name), len(DayLayout))]
		if _, err := time.Parse(DayLayout, day); err != nil {
			continue
		}
		if strings.HasPrefix(name[len(day):], "-") {
			a.Raw[day] = 24
		} else if a.Raw[day] == 0 {
			a.Raw[day] = 1
		}
	}
	p.state.SetMeta("archive", a)

	if len(snaps) == 0 {
		first := ""
		for d := range a.Raw {
			if first == "" || d < first {
				first = d
			}
		}
		progress("no state snapshot: indexing every archived raw day from " + first)
		return true, p.rebuild(ctx, dayTime(first), progress, false)
	}

	latest := snaps[len(snaps)-1]
	snapDay := strings.TrimSuffix(latest[strings.LastIndex(latest, "/")+1:], ".ndjson.zst")
	if _, err := time.Parse(DayLayout, snapDay); err != nil {
		return false, fmt.Errorf("snapshot %s: not a day", latest)
	}
	body, err := c.Get(ctx, latest)
	if err != nil {
		return false, err
	}
	raw, err := decompress(body)
	if err != nil {
		return false, fmt.Errorf("snapshot %s: %w", snapDay, err)
	}

	rolls, err := c.List(ctx, base+"/rollup/")
	if err != nil {
		return false, err
	}
	got := 0
	for _, k := range rolls {
		day := strings.TrimSuffix(k[strings.LastIndex(k, "/")+1:], ".roll.zst")
		if _, err := time.Parse(DayLayout, day); err != nil {
			continue
		}
		if _, err := os.Stat(rollupPath(p.dir, day)); err == nil {
			continue
		}
		b, err := c.Get(ctx, k)
		if err != nil {
			return false, err
		}
		if _, err := decodeRollup(b); err != nil {
			return false, fmt.Errorf("archived rollup %s: %w", day, err)
		}
		if err := writeFileAtomic(rollupPath(p.dir, day), b); err != nil {
			return false, err
		}
		got++
	}
	progress(fmt.Sprintf("%d rollups from the archive", got))

	p.indexMu.Lock()
	if !p.state.Empty() {
		p.indexMu.Unlock()
		return false, errors.New("the index is not empty: a restore only fills a new or lost one (use rebuild)")
	}
	err = p.state.ReadSnapshot(bytes.NewReader(raw))
	p.indexMu.Unlock()
	if err != nil {
		return false, fmt.Errorf("snapshot %s: %w", snapDay, err)
	}
	progress("index from the snapshot of " + snapDay)
	// The snapshot was taken during its day: what was received from the day before on is
	// applied again (indexing is idempotent) and its rollups recomputed.
	return true, p.rebuild(ctx, dayTime(snapDay).AddDate(0, 0, -1), progress, true)
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
