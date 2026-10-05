package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s := &Store{BufDir: filepath.Join(dir, "buf"), DataDir: filepath.Join(dir, "data"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func lines(s *Store, k Key, q Query) []string {
	var out []string
	s.Read(k, q, func(l Line) { out = append(out, l.L) })
	return out
}

func TestStoreFlushReadTail(t *testing.T) {
	s := newStore(t)
	k := Key{"shop", KindApp, "api"}
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 10; i++ {
		s.Append(k, Line{T: base.Add(time.Duration(i) * time.Second).UnixNano(), L: fmt.Sprint("a", i)})
	}
	if err := s.Flush(k); err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 15; i++ {
		s.Append(k, Line{T: base.Add(time.Duration(i) * time.Second).UnixNano(), L: fmt.Sprint("a", i)})
	}
	all := lines(s, k, Query{})
	if len(all) != 15 || all[0] != "a0" || all[14] != "a14" {
		t.Fatalf("all: %v", all)
	}
	tail := lines(s, k, Query{Tail: 7})
	if strings.Join(tail, ",") != "a8,a9,a10,a11,a12,a13,a14" {
		t.Fatalf("tail: %v", tail)
	}
	since := lines(s, k, Query{Since: base.Add(12 * time.Second)})
	if strings.Join(since, ",") != "a12,a13,a14" {
		t.Fatalf("since: %v", since)
	}
	grep := lines(s, k, Query{Match: func(l Line) bool { return strings.HasSuffix(l.L, "1") }})
	if strings.Join(grep, ",") != "a1,a11" {
		t.Fatalf("grep: %v", grep)
	}
	// Data on disk is compressed, buffer is empty after flush.
	s.FlushAll()
	if fi, _ := os.Stat(filepath.Join(s.BufDir, k.path()+".wal")); fi.Size() != 0 {
		t.Fatal("wal not truncated")
	}
	if s.DiskUsage(k) == 0 {
		t.Fatal("nothing on disk")
	}
}

func TestStoreSurvivesRestart(t *testing.T) {
	s := newStore(t)
	k := Key{"shop", KindApp, "api"}
	s.Append(k, Line{T: time.Now().UnixNano(), L: "before crash"})
	// Simulate a crash: files closed, nothing flushed.
	for _, st := range s.streams {
		st.wal.Close()
	}
	s.streams = map[Key]*stream{}
	// New process over the same dirs: the tmpfs buffer is recovered to disk.
	s2 := &Store{BufDir: s.BufDir, DataDir: s.DataDir, Log: s.Log}
	if err := s2.Open(); err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := lines(s2, k, Query{}); len(got) != 1 || got[0] != "before crash" {
		t.Fatal(got)
	}
	if s2.DiskUsage(k) == 0 {
		t.Fatal("not flushed on open")
	}
}

func TestStoreRetention(t *testing.T) {
	s := newStore(t)
	k := Key{"shop", KindApp, "api"}
	dir := filepath.Join(s.DataDir, k.path())
	os.MkdirAll(dir, 0o755)
	old := time.Now().Add(-20 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, segmentName(old.Add(time.Duration(i)*time.Hour))), make([]byte, 100), 0o644)
	}
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, segmentName(time.Now().Add(-time.Duration(i)*time.Hour))), make([]byte, 100), 0o644)
	}
	s.RetentionFor = func(Key) Retention { return Retention{Age: 14 * 24 * time.Hour, Max: 350} }
	s.Cleanup()
	if n := len(s.segments(k)); n != 3 {
		t.Fatalf("segments left: %d", n)
	}
}

func TestSubscribe(t *testing.T) {
	s := newStore(t)
	k := Key{"shop", KindApp, "api"}
	ch, cancel, _ := s.Subscribe(k)
	defer cancel()
	s.Append(k, Line{L: "live"})
	select {
	case l := <-ch:
		if l.L != "live" {
			t.Fatal(l)
		}
	case <-time.After(time.Second):
		t.Fatal("no live line")
	}
}

// Forward protocol, as sent by docker's fluentd driver.

func record(w *mpWriter, source, log string, extra ...string) {
	w.mapHeader(2 + len(extra)/2)
	w.str("source")
	w.str(source)
	w.str("log")
	w.str(log)
	for i := 0; i+1 < len(extra); i += 2 {
		w.str(extra[i])
		w.str(extra[i+1])
	}
}

func TestForward(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := &ForwardServer{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Handle: func(tag string, r Record) {
		mu.Lock()
		got = append(got, fmt.Sprintf("%s|%v|%s", tag, r.Stderr, r.Line))
		mu.Unlock()
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Now()

	// Message mode with ack.
	w := &mpWriter{}
	w.arrHeader(4)
	w.str("shop.api.3.1")
	w.eventTime(now)
	record(w, "stdout", "hello\n")
	w.mapHeader(1)
	w.str("chunk")
	w.str("abc")
	// Forward mode with a split (partial) line.
	w.arrHeader(2)
	w.str("shop.api.3.1")
	w.arrHeader(2)
	w.arrHeader(2)
	w.int(now.Unix())
	record(w, "stderr", "par", "partial_message", "true", "partial_id", "p1", "partial_ordinal", "1")
	w.arrHeader(2)
	w.int(now.Unix())
	record(w, "stderr", "tial\n", "partial_message", "true", "partial_id", "p1", "partial_ordinal", "2", "partial_last", "true")
	// PackedForward, gzip.
	inner := &mpWriter{}
	inner.arrHeader(2)
	inner.eventTime(now)
	record(inner, "stdout", "packed")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(inner.b)
	zw.Close()
	w.arrHeader(3)
	w.str("shop.web.1.2")
	w.b = append(w.b, 0xc6, 0, 0, byte(gz.Len()>>8), byte(gz.Len()))
	w.b = append(w.b, gz.Bytes()...)
	w.mapHeader(1)
	w.str("compressed")
	w.str("gzip")
	c.Write(w.b)

	// Ack for the first message.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := &mpReader{r: bufio.NewReader(c)}
	ack, err := r.decode()
	if err != nil || ack.(map[string]any)["ack"] != "abc" {
		t.Fatalf("ack %v %v", ack, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := "shop.api.3.1|false|hello;shop.api.3.1|true|partial;shop.web.1.2|false|packed"
	if strings.Join(got, ";") != want {
		t.Fatalf("got %v", got)
	}
}

func TestParseSyslog(t *testing.T) {
	tag, msg, sev, ok := parseSyslog(`<190>Oct  5 18:00:00 access_shop: {"status":200}`)
	if !ok || tag != "access_shop" || msg != `{"status":200}` || sev != 6 {
		t.Fatal(tag, msg, sev, ok)
	}
	tag, _, sev, ok = parseSyslog(`<187>Oct 15 18:00:00 nginx: connect() failed`)
	if !ok || tag != "nginx" || sev != 3 {
		t.Fatal(tag, sev, ok)
	}
	for _, bad := range []string{"", "hello", "<x>Oct  5 18:00:00 a: b", "<190>no colon"} {
		if _, _, _, ok := parseSyslog(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func FuzzMsgpack(f *testing.F) {
	w := &mpWriter{}
	w.arrHeader(3)
	w.str("t")
	w.eventTime(time.Now())
	record(w, "stdout", "x")
	f.Add(w.b)
	f.Fuzz(func(t *testing.T, b []byte) {
		r := &mpReader{r: bufio.NewReader(bytes.NewReader(b))}
		for i := 0; i < 100; i++ {
			if _, err := r.decode(); err != nil {
				return
			}
		}
	})
}
