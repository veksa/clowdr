// Package logs collects container and nginx logs without touching the disk for every line:
// lines go to a write-ahead file in tmpfs (RAM, but it survives a clowdrd restart) and are
// flushed to disk once a minute (or at 4 MB) as zstd-compressed hourly segments.
package logs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Key names a stream: an app stream per service, one access stream per project.
type Key struct {
	Project string
	Kind    string // KindApp | KindAccess | KindNginx
	Name    string // service; "_" for access and nginx
}

const (
	KindApp    = "app"
	KindAccess = "access"
	KindNginx  = "nginx"
)

func (k Key) path() string { return filepath.Join(k.Project, k.Kind, k.Name) }

func (k Key) String() string { return k.Project + "/" + k.Kind + "/" + k.Name }

// Line is the stored form of a log line.
type Line struct {
	T int64  `json:"t"`           // unix nanoseconds
	R uint64 `json:"r,omitempty"` // release
	I int    `json:"i,omitempty"` // replica
	E bool   `json:"e,omitempty"` // stderr
	L string `json:"l"`
}

func (l Line) Time() time.Time { return time.Unix(0, l.T) }

type Retention struct {
	Age time.Duration
	Max int64 // bytes on disk (compressed)
}

type Store struct {
	BufDir     string // tmpfs, e.g. /run/clowdr/buf
	DataDir    string // e.g. /var/lib/clowdr/logs
	FlushEvery time.Duration
	FlushBytes int64 // per stream
	BufLimit   int64 // all streams together
	// RetentionFor returns the rule for a stream.
	RetentionFor func(Key) Retention
	Log          *slog.Logger

	mu       sync.Mutex
	streams  map[Key]*stream
	enc      *zstd.Encoder
	bufTotal int64
	kick     chan struct{}
}

type stream struct {
	key  Key
	mu   sync.Mutex
	wal  *os.File
	size int64
	subs map[chan Line]struct{}
}

func (s *Store) init() error {
	if s.FlushEvery == 0 {
		s.FlushEvery = time.Minute
	}
	if s.FlushBytes == 0 {
		s.FlushBytes = 4 << 20
	}
	if s.BufLimit == 0 {
		s.BufLimit = 16 << 20
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	s.streams = map[Key]*stream{}
	s.kick = make(chan struct{}, 1)
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	s.enc = enc
	return nil
}

// Open prepares the store and flushes buffers left by a previous run (clowdrd restart).
func (s *Store) Open() error {
	if err := s.init(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.BufDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.DataDir, 0o750); err != nil {
		return err
	}
	return filepath.WalkDir(s.BufDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(p, ".wal") && !strings.HasSuffix(p, ".flushing") {
			return nil
		}
		rel, _ := filepath.Rel(s.BufDir, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		k := Key{Project: parts[0], Kind: parts[1], Name: strings.TrimSuffix(strings.TrimSuffix(parts[2], ".wal"), ".flushing")}
		if err := s.writeSegment(k, p); err != nil {
			s.Log.Error("logs: recover buffer", "file", p, "err", err)
		}
		return nil
	})
}

func (s *Store) get(k Key) (*stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[k]
	if st != nil {
		return st, nil
	}
	p := filepath.Join(s.BufDir, k.path()+".wal")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, _ := f.Stat()
	st = &stream{key: k, wal: f, size: fi.Size(), subs: map[chan Line]struct{}{}}
	s.streams[k] = st
	return st, nil
}

// Append adds lines to a stream: one write to tmpfs, then live subscribers.
func (s *Store) Append(k Key, lines ...Line) error {
	st, err := s.get(k)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		enc.Encode(l)
	}
	st.mu.Lock()
	n, err := st.wal.Write(buf.Bytes())
	st.size += int64(n)
	size := st.size
	for ch := range st.subs {
		for _, l := range lines {
			select {
			case ch <- l:
			default: // slow reader: drop rather than block logging
			}
		}
	}
	st.mu.Unlock()
	s.mu.Lock()
	s.bufTotal += int64(n)
	over := size >= s.FlushBytes || s.bufTotal >= s.BufLimit
	s.mu.Unlock()
	if over {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return err
}

// Subscribe streams new lines of k until cancel is called.
func (s *Store) Subscribe(k Key) (<-chan Line, func(), error) {
	st, err := s.get(k)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan Line, 1024)
	st.mu.Lock()
	st.subs[ch] = struct{}{}
	st.mu.Unlock()
	return ch, func() {
		st.mu.Lock()
		delete(st.subs, ch)
		st.mu.Unlock()
	}, nil
}

// Run flushes periodically until stop is closed, then flushes everything once more.
func (s *Store) Run(stop <-chan struct{}) {
	t := time.NewTicker(s.FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			s.FlushAll()
			return
		case <-t.C:
			s.FlushAll()
			s.Cleanup()
		case <-s.kick:
			s.FlushAll()
		}
	}
}

// FlushAll writes every non-empty buffer to disk.
func (s *Store) FlushAll() {
	s.mu.Lock()
	keys := make([]Key, 0, len(s.streams))
	for k := range s.streams {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	for _, k := range keys {
		if err := s.Flush(k); err != nil {
			s.Log.Error("logs: flush", "stream", k, "err", err)
		}
	}
}

// Flush moves one stream's buffer to its disk segment (e.g. right after a crash, so the last
// lines of a dead container are on disk at once).
func (s *Store) Flush(k Key) error {
	s.mu.Lock()
	st := s.streams[k]
	s.mu.Unlock()
	if st == nil {
		return nil
	}
	st.mu.Lock()
	if st.size == 0 {
		st.mu.Unlock()
		return nil
	}
	walPath := filepath.Join(s.BufDir, k.path()+".wal")
	flushing := filepath.Join(s.BufDir, k.path()+".flushing")
	st.wal.Close()
	if err := os.Rename(walPath, flushing); err != nil {
		st.wal, _ = os.OpenFile(walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		st.mu.Unlock()
		return err
	}
	f, err := os.OpenFile(walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		st.mu.Unlock()
		return err
	}
	st.wal = f
	size := st.size
	st.size = 0
	st.mu.Unlock()
	s.mu.Lock()
	s.bufTotal -= size
	s.mu.Unlock()
	return s.writeSegment(k, flushing)
}

func segmentName(t time.Time) string { return t.UTC().Format("2006010215") + ".zst" }

// writeSegment compresses a buffer file into one zstd frame appended to the hourly segment.
func (s *Store) writeSegment(k Key, bufFile string) error {
	raw, err := os.ReadFile(bufFile)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		dir := filepath.Join(s.DataDir, k.path())
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		frame := s.enc.EncodeAll(raw, nil)
		f, err := os.OpenFile(filepath.Join(dir, segmentName(time.Now())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return err
		}
		_, err = f.Write(frame)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return os.Remove(bufFile)
}

// Cleanup deletes whole segments older than the retention or beyond the size limit.
func (s *Store) Cleanup() {
	if s.RetentionFor == nil {
		return
	}
	projects, _ := os.ReadDir(s.DataDir)
	for _, p := range projects {
		kinds, _ := os.ReadDir(filepath.Join(s.DataDir, p.Name()))
		for _, kd := range kinds {
			names, _ := os.ReadDir(filepath.Join(s.DataDir, p.Name(), kd.Name()))
			for _, n := range names {
				k := Key{Project: p.Name(), Kind: kd.Name(), Name: n.Name()}
				s.cleanupStream(k, s.RetentionFor(k))
			}
		}
	}
}

func (s *Store) cleanupStream(k Key, r Retention) {
	segs := s.segments(k)
	var total int64
	for i := len(segs) - 1; i >= 0; i-- { // newest first
		seg := segs[i]
		total += seg.size
		tooOld := r.Age > 0 && time.Since(seg.hour) > r.Age+time.Hour
		tooBig := r.Max > 0 && total > r.Max && i != len(segs)-1
		if tooOld || tooBig {
			os.Remove(seg.path)
		}
	}
}

type segment struct {
	path string
	hour time.Time
	size int64
}

// segments lists a stream's segments, oldest first.
func (s *Store) segments(k Key) []segment {
	dir := filepath.Join(s.DataDir, k.path())
	es, _ := os.ReadDir(dir)
	var out []segment
	for _, e := range es {
		h, err := time.Parse("2006010215", strings.TrimSuffix(e.Name(), ".zst"))
		if err != nil || !strings.HasSuffix(e.Name(), ".zst") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, segment{path: filepath.Join(dir, e.Name()), hour: h, size: fi.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].hour.Before(out[j].hour) })
	return out
}

// Query reads lines of a stream in time order. Since/Until bound the time (zero = open),
// Match filters, Tail keeps only the last N matching lines (0 = all).
type Query struct {
	Since, Until time.Time
	Match        func(Line) bool
	Tail         int
}

func (s *Store) Read(k Key, q Query, fn func(Line)) error {
	keep := func(l Line) bool {
		t := l.Time()
		if !q.Since.IsZero() && t.Before(q.Since) {
			return false
		}
		if !q.Until.IsZero() && t.After(q.Until) {
			return false
		}
		return q.Match == nil || q.Match(l)
	}
	var sources []func(func(Line)) error
	for _, seg := range s.segments(k) {
		// A segment holds what was flushed during its hour, so lines can be up to one flush
		// interval (or a clowdrd outage) older; one hour of slack covers it.
		if !q.Since.IsZero() && seg.hour.Add(2*time.Hour).Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && seg.hour.Add(-time.Hour).After(q.Until) {
			continue
		}
		path := seg.path
		sources = append(sources, func(fn func(Line)) error { return readZstd(path, fn) })
	}
	for _, suffix := range []string{".flushing", ".wal"} {
		path := filepath.Join(s.BufDir, k.path()+suffix)
		sources = append(sources, func(fn func(Line)) error { return readPlain(path, fn) })
	}

	if q.Tail <= 0 {
		for _, src := range sources {
			if err := src(func(l Line) {
				if keep(l) {
					fn(l)
				}
			}); err != nil {
				return err
			}
		}
		return nil
	}
	// Tail: walk sources newest first, stop once enough lines are collected.
	var chunks [][]Line
	n := 0
	for i := len(sources) - 1; i >= 0 && n < q.Tail; i-- {
		var c []Line
		if err := sources[i](func(l Line) {
			if keep(l) {
				c = append(c, l)
			}
		}); err != nil {
			return err
		}
		chunks = append(chunks, c)
		n += len(c)
	}
	var all []Line
	for i := len(chunks) - 1; i >= 0; i-- {
		all = append(all, chunks[i]...)
	}
	if len(all) > q.Tail {
		all = all[len(all)-q.Tail:]
	}
	for _, l := range all {
		fn(l)
	}
	return nil
}

func readPlain(path string, fn func(Line)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	return scanLines(f, fn)
}

func readZstd(path string, fn func(Line)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	d, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer d.Close()
	return scanLines(d, fn)
}

func scanLines(r io.Reader, fn func(Line)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l Line
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			fn(l)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read logs: %w", err)
	}
	return nil
}

// Remove deletes everything of a project or of one stream (service removed).
func (s *Store) Remove(k Key) {
	s.mu.Lock()
	for sk, st := range s.streams {
		if sk.Project == k.Project && (k.Kind == "" || sk == k) {
			st.wal.Close()
			delete(s.streams, sk)
		}
	}
	s.mu.Unlock()
	p := k.Project
	if k.Kind != "" {
		p = k.path()
	}
	os.RemoveAll(filepath.Join(s.BufDir, p))
	os.RemoveAll(filepath.Join(s.DataDir, p))
}

// DiskUsage returns bytes used by a stream on disk.
func (s *Store) DiskUsage(k Key) int64 {
	var n int64
	for _, seg := range s.segments(k) {
		n += seg.size
	}
	return n
}

// Close flushes all buffers and closes the files.
func (s *Store) Close() error {
	s.FlushAll()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, st := range s.streams {
		st.wal.Close()
		delete(s.streams, k)
	}
	return nil
}
