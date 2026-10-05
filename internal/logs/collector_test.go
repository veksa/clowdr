package logs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"
)

func TestCollectorRoutes(t *testing.T) {
	s := newStore(t)
	var seen []Key
	c := &Collector{Store: s, Projects: func() []string { return []string{"my-shop", "blog"} },
		OnLine: func(k Key, _ Line) { seen = append(seen, k) }}
	now := time.Now()

	c.Forward(Tag("my-shop", "web", 3, 2), Record{Time: now, Stderr: true, Line: "boom"})
	c.Forward("garbage", Record{Time: now, Line: "who am I"})
	c.Syslog(SyslogTag("my-shop"), Record{Time: now, Line: `{"status":200}`})
	c.Syslog(SyslogTag("blog"), Record{Time: now, Line: `{"status":404}`})
	c.Syslog("nginx", Record{Time: now, Stderr: true, Line: "connect() failed"})

	want := []Key{
		{"my-shop", KindApp, "web"},
		{Platform, KindApp, "unknown"},
		{"my-shop", KindAccess, "_"}, // '-' in the project name survives the syslog tag
		{"blog", KindAccess, "_"},
		{Platform, KindNginx, "_"},
	}
	if len(seen) != len(want) {
		t.Fatalf("seen %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("line %d went to %v, want %v", i, seen[i], want[i])
		}
	}
	s.FlushAll()
	var got Line
	s.Read(Key{"my-shop", KindApp, "web"}, Query{}, func(l Line) { got = l })
	if got.R != 3 || got.I != 2 || !got.E || got.L != "boom" {
		t.Fatalf("%+v", got)
	}
	if l := lines(s, Key{Platform, KindApp, "unknown"}, Query{}); len(l) != 1 || l[0] != "garbage: who am I" {
		t.Fatal(l)
	}
}

func TestRemoveAndTrimOldest(t *testing.T) {
	s := newStore(t)
	web, api := Key{"shop", KindApp, "web"}, Key{"shop", KindApp, "api"}
	blog := Key{"blog", KindAccess, "_"}
	for _, k := range []Key{web, api, blog} {
		s.Append(k, Line{T: time.Now().UnixNano(), L: "x"})
		s.Flush(k)
	}
	s.Remove(web)
	if len(lines(s, web, Query{})) != 0 || len(lines(s, api, Query{})) != 1 {
		t.Fatal("Remove(stream) must remove exactly that stream")
	}
	s.Remove(Key{Project: "shop"})
	if _, err := os.Stat(filepath.Join(s.DataDir, "shop")); !os.IsNotExist(err) {
		t.Fatal("Remove(project) left data")
	}
	if len(lines(s, blog, Query{})) != 1 {
		t.Fatal("another project's logs were removed")
	}

	// Low disk: the oldest hours go first, across streams; the current hour stays.
	dir := filepath.Join(s.DataDir, blog.path())
	old := time.Now().Add(-48 * time.Hour)
	for i := range 3 {
		os.WriteFile(filepath.Join(dir, segmentName(old.Add(time.Duration(i)*time.Hour))), make([]byte, 100), 0o644)
	}
	if freed := s.TrimOldest(150); freed != 200 {
		t.Fatalf("freed %d, want the two oldest segments (200)", freed)
	}
	if freed := s.TrimOldest(1 << 30); freed != 100 {
		t.Fatalf("freed %d, want only the last old segment", freed)
	}
	if len(lines(s, blog, Query{})) != 1 {
		t.Fatal("the current hour was trimmed")
	}
}

func TestRunFlushesOnKickAndStop(t *testing.T) {
	s := newStore(t)
	s.FlushBytes = 10 // a full buffer kicks a flush before the minute is up
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { s.Run(stop); close(done) }()
	k := Key{"shop", KindApp, "web"}
	s.Append(k, Line{T: time.Now().UnixNano(), L: "a line longer than ten bytes"})
	deadline := time.Now().Add(5 * time.Second)
	for s.DiskUsage(k) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a full buffer was not flushed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.FlushBytes = 1 << 20
	before := s.DiskUsage(k)
	s.Append(k, Line{T: time.Now().UnixNano(), L: "small"})
	close(stop)
	<-done
	if s.DiskUsage(k) == before {
		t.Fatal("stop did not flush")
	}
}

func TestSyslogServer(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("unix datagram sockets")
	}
	sock := filepath.Join(t.TempDir(), "log.sock")
	c, err := ListenSyslog(sock)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- (&SyslogServer{Handle: func(tag string, r Record) { got <- tag + " " + r.Line }}).Serve(ctx, c)
	}()
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode %v: nginx workers cannot write", st.Mode())
	}
	w, err := net.Dial("unixgram", sock)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(`<190>Oct  5 18:00:00 access_shop: {"status":200}`))
	w.Write([]byte(`not syslog`))
	w.Close()
	select {
	case s := <-got:
		if s != `access_shop {"status":200}` {
			t.Fatal(s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
