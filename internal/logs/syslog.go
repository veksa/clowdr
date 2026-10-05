package logs

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// SyslogServer receives nginx access and error logs on a unix datagram socket
// (`access_log syslog:server=unix:/run/clowdr/log.sock,tag=...,nohostname`).
type SyslogServer struct {
	// Handle gets the syslog tag (e.g. "access_shop" or "nginx") and the message.
	Handle func(tag string, r Record)
}

// ListenSyslog creates the socket; nginx workers run unprivileged, so it is world-writable.
func ListenSyslog(path string) (*net.UnixConn, error) {
	os.Remove(path)
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o666)
	c.SetReadBuffer(4 << 20)
	return c, nil
}

func (s *SyslogServer) Serve(ctx context.Context, c *net.UnixConn) error {
	go func() { <-ctx.Done(); c.Close() }()
	buf := make([]byte, 64<<10)
	for {
		n, _, err := c.ReadFromUnix(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		tag, msg, sev, ok := parseSyslog(string(buf[:n]))
		if ok {
			s.Handle(tag, Record{Time: time.Now(), Stderr: sev <= 4, Line: msg})
		}
	}
}

// parseSyslog parses "<PRI>Mmm dd hh:mm:ss tag: message" (RFC 3164 without hostname).
func parseSyslog(s string) (tag, msg string, severity int, ok bool) {
	if !strings.HasPrefix(s, "<") {
		return "", "", 0, false
	}
	end := strings.IndexByte(s, '>')
	if end < 2 || end > 4 {
		return "", "", 0, false
	}
	pri := 0
	for _, c := range s[1:end] {
		if c < '0' || c > '9' {
			return "", "", 0, false
		}
		pri = pri*10 + int(c-'0')
	}
	rest := s[end+1:]
	if len(rest) > 16 && rest[3] == ' ' && rest[15] == ' ' { // "Oct  5 18:00:00 "
		rest = rest[16:]
	}
	tag, msg, ok = strings.Cut(rest, ": ")
	if !ok || strings.ContainsAny(tag, " ") {
		return "", "", 0, false
	}
	return tag, strings.TrimRight(msg, "\n"), pri % 8, true
}
