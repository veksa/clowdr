package analytics

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/veksa/clowdr/internal/config"
)

func importRecord(id, ts, event, user, device string) string {
	l := fmt.Sprintf(`{"$":"analytics","v":1,"id":%q,"ts":%q,"e":%q`, id, ts, event)
	if user != "" {
		l += fmt.Sprintf(`,"u":%q`, user)
	}
	if device != "" {
		l += fmt.Sprintf(`,"d":%q`, device)
	}
	return l + `,"app":"mobile","pf":"ios","env":"prod","src":"import"}`
}

func importEnv(t *testing.T, now time.Time) (*env, *fakeS3) {
	f := newFakeS3(t)
	e := newEnv(t, now)
	e.m.HTTP = f.srv.Client()
	e.configure(config.AnalyticsConf{Archive: &config.ArchiveConf{Endpoint: f.srv.URL, Region: "auto", Bucket: "bucket"}},
		map[string]string{SecretS3Key: "key", SecretS3Secret: f.secret, SecretS3Prefix: "analytics/00ff"})
	return e, f
}

func archivedLines(t *testing.T, f *fakeS3, day string) []string {
	t.Helper()
	b, ok := f.objects[RawKey("analytics/00ff/v1/shop", day, -1)]
	if !ok {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(mustDecompress(t, b))), "\n")
}

// History goes into the archived raw days of its event days, merged with what is there and
// deduplicated by id; the rollups count it; the same file again changes nothing.
func TestImportMergesIntoTheArchiveAndRebuilds(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	e, f := importEnv(t, now)
	p := e.project()
	ctx := context.Background()
	// A day the archive has already: one record, which the import repeats.
	there := importRecord("ev-1", "2026-10-01T10:00:00.000Z", "episode_viewed", "u_a", "")
	f.objects[RawKey("analytics/00ff/v1/shop", "2026-10-01", -1)] = compressBest([]byte(there + "\n"))
	lines := []string{
		importRecord("ev-2", "2026-10-01T09:00:00.000Z", "episode_viewed", "", "dev-0001"),
		there,
		importRecord("ev-3", "2026-10-02T10:00:00.000Z", "episode_viewed", "u_b", ""),
		importRecord("ev-3", "2026-10-02T10:00:00.000Z", "episode_viewed", "u_b", ""), // twice in the file
		importRecord("ev-4", "2026-11-16T08:00:00.000Z", "episode_viewed", "u_c", ""), // today: the stream's
		`{"$":"analytics","v":1,"id":"ev-5","ts":"yesterday","e":"episode_viewed","u":"u_d"}`,
		"not a record",
	}
	var msgs []string
	rep, err := p.Import(ctx, lines, ImportOptions{}, func(m string) { msgs = append(msgs, m) })
	if err != nil {
		t.Fatal(err)
	}
	if rep.Records != 6 || rep.Invalid != 2 || rep.Imported != 2 || rep.Present != 2 || rep.SkippedRecords != 1 ||
		strings.Join(rep.Days, ",") != "2026-10-01,2026-10-02" || strings.Join(rep.Skipped, ",") != "2026-11-16" {
		t.Fatalf("report %+v", rep)
	}
	day1 := archivedLines(t, f, "2026-10-01")
	if len(day1) != 2 || !strings.Contains(day1[0], `"ev-2"`) || !strings.Contains(day1[1], `"ev-1"`) {
		t.Fatalf("2026-10-01 is not merged in time order: %q", day1)
	}
	if got := archivedLines(t, f, "2026-10-02"); len(got) != 1 {
		t.Fatalf("2026-10-02: %q", got)
	}
	if archivedLines(t, f, "2026-11-16") != nil {
		t.Fatal("a day of the stream was written to the archive")
	}
	if a := p.archiveState(); a.Raw["2026-10-01"] != 1 || a.Raw["2026-10-02"] != 1 {
		t.Fatalf("archive state %+v", a.Raw)
	}
	res := e.query(Spec{Kind: "trends", Events: []string{"episode_viewed"}, Since: "2026-10-01", Until: "2026-10-02T23:59:59Z", Interval: "day"})
	if res.Series[0].Total != 3 {
		t.Fatalf("the rollups count %v events, want 3: %+v", res.Series[0].Total, res.Series[0])
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "left out") {
		t.Fatalf("the skipped day was not reported: %q", msgs)
	}

	again, err := p.Import(ctx, lines, ImportOptions{}, nil)
	if err != nil || again.Imported != 0 || again.Present != 4 || len(again.Days) != 0 {
		t.Fatalf("a second import: %+v %v", again, err)
	}
}

// Days from the first day of the stream on are clowdr's own: an import leaves them out.
func TestImportLeavesOutDaysOfTheStream(t *testing.T) {
	// The store names segments by the wall clock: the stream begins today, three days before
	// the manager's now.
	wall := time.Now().UTC()
	first := wall.Format(DayLayout)
	before := wall.AddDate(0, 0, -1).Format(DayLayout)
	e, f := importEnv(t, wall.AddDate(0, 0, 3))
	e.feed(wall, importRecord("live-1", wall.Format(time.RFC3339Nano), "episode_viewed", "u_a", ""))
	e.ls.FlushAll()
	p := e.project()
	if got := p.ImportCutoff(); got != first {
		t.Fatalf("cutoff %s, want %s", got, first)
	}
	rep, err := p.Import(context.Background(), []string{
		importRecord("old-1", before+"T10:00:00.000Z", "episode_viewed", "u_a", ""),
		importRecord("old-2", first+"T00:00:01.000Z", "episode_viewed", "u_a", ""),
	}, ImportOptions{}, nil)
	if err != nil || rep.Imported != 1 || strings.Join(rep.Skipped, ",") != first {
		t.Fatalf("%+v %v", rep, err)
	}
	if archivedLines(t, f, first) != nil || len(archivedLines(t, f, before)) != 1 {
		t.Fatal("the import wrote the wrong days")
	}
}

// Asked for, records of days the stream covers go into it as late records: deduplicated by id
// against the stream, their days recomputed; days past the late window are refused.
func TestImportIntoLiveDays(t *testing.T) {
	// The store names segments by the wall clock: the stream begins today, ten days before the
	// manager's now. Its first day is past the late window, five days later is inside it.
	wall := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	first := wall.Format(DayLayout)
	inside := wall.AddDate(0, 0, 5).Format(DayLayout)
	e, f := importEnv(t, wall.AddDate(0, 0, 10))
	e.feed(wall, importRecord("live-1", first+"T12:00:00.000Z", "episode_viewed", "u_a", ""))
	e.ls.FlushAll() // the first segment; later lines stay in the buffer (a segment is named by the wall clock)
	e.feed(time.Time{}, importRecord("live-2", inside+"T09:00:00.000Z", "episode_viewed", "u_b", ""))
	p := e.project()
	ctx := context.Background()
	if _, err := p.Recompute([]string{inside}); err != nil {
		t.Fatal(err)
	}
	tail := []string{
		importRecord("tail-1", inside+"T10:00:00.000Z", "episode_viewed", "", "dev-0003"),
		importRecord("live-2", inside+"T09:00:00.000Z", "episode_viewed", "u_b", ""), // the stream has it
		importRecord("tail-2", first+"T13:00:00.000Z", "episode_viewed", "u_c", ""),  // past the late window
	}

	plain, err := p.Import(ctx, tail, ImportOptions{}, nil)
	if err != nil || plain.Imported != 0 || plain.SkippedRecords != 3 {
		t.Fatalf("without live days: %+v %v", plain, err)
	}

	var msgs []string
	rep, err := p.Import(ctx, tail, ImportOptions{LiveDays: true}, func(m string) { msgs = append(msgs, m) })
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 1 || rep.Present != 1 || strings.Join(rep.Live, ",") != inside ||
		strings.Join(rep.TooLate, ",") != first || rep.TooLateRecords != 1 || len(rep.Days) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if len(f.objects) != 0 {
		t.Fatalf("live days were written to the archive: %d objects", len(f.objects))
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "late window") {
		t.Fatalf("the refused day was not reported: %q", msgs)
	}
	res := e.query(Spec{Kind: "trends", Events: []string{"episode_viewed"}, Since: inside, Until: inside + "T23:59:59Z", Interval: "day"})
	if res.Series[0].Total != 2 {
		t.Fatalf("the recomputed day counts %v events, want 2", res.Series[0].Total)
	}

	again, err := p.Import(ctx, tail, ImportOptions{LiveDays: true}, nil)
	if err != nil || again.Imported != 0 || again.Present != 2 {
		t.Fatalf("a second import: %+v %v", again, err)
	}
	if res := e.query(Spec{Kind: "trends", Events: []string{"episode_viewed"}, Since: inside, Until: inside + "T23:59:59Z", Interval: "day"}); res.Series[0].Total != 2 {
		t.Fatalf("after a second import the day counts %v", res.Series[0].Total)
	}
}

func TestImportNeedsTheArchive(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
	if _, err := e.project().Import(context.Background(), []string{importRecord("x-1", "2026-10-01T10:00:00.000Z", "a_b", "u_a", "")}, ImportOptions{}, nil); err == nil ||
		!strings.Contains(err.Error(), "archive") {
		t.Fatalf("imported without an archive: %v", err)
	}
}

// An import file is NDJSON or NDJSON+zstd; check reads both.
func TestImportFilesMayBeCompressed(t *testing.T) {
	plain := importRecord("z-1", "2026-10-01T10:00:00.000Z", "episode_viewed", "u_a", "") + "\n" +
		importRecord("z-2", "2026-10-01T11:00:00.000Z", "episode_viewed", "", "dev-0002") + "\n"
	enc, _ := zstd.NewWriter(nil)
	packed := enc.EncodeAll([]byte(plain), nil)
	for _, body := range [][]byte{[]byte(plain), packed} {
		lines, err := ReadImport(bytes.NewReader(body))
		if err != nil || len(lines) != 2 {
			t.Fatalf("%d lines, %v", len(lines), err)
		}
	}
	path := filepath.Join(t.TempDir(), "history.ndjson.zst")
	if err := os.WriteFile(path, packed, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := CheckFiles([]string{path})
	if err != nil || rep.Records != 2 || rep.Invalid != 0 || rep.Warnings != 0 || rep.Events["episode_viewed"].N != 2 {
		t.Fatalf("check of a compressed file: %+v %v", rep, err)
	}
}
