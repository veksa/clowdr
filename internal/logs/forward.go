package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Record is one log line from a container.
type Record struct {
	Time   time.Time
	Stderr bool
	Line   string
}

// ForwardServer accepts the fluentd forward protocol, which docker's `fluentd` log driver
// speaks, so no fluentd is needed: containers log straight into clowdrd.
type ForwardServer struct {
	// Handle receives every line with the tag set on the container ("project.service.release.replica").
	Handle func(tag string, r Record)
	Log    *slog.Logger

	mu sync.Mutex
	ln net.Listener
}

func (s *ForwardServer) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.conn(c)
	}
}

func (s *ForwardServer) conn(c net.Conn) {
	defer c.Close()
	r := &mpReader{r: bufio.NewReaderSize(c, 64<<10)}
	parts := map[string]*strings.Builder{} // docker splits lines > 16K into partial messages
	for {
		v, err := r.decode()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.Log.Debug("fluentd forward", "err", err)
			}
			return
		}
		msg, ok := v.([]any)
		if !ok || len(msg) < 2 {
			continue
		}
		tag, _ := msg[0].(string)
		var option map[string]any
		switch body := msg[1].(type) {
		case []any: // Forward mode: [tag, [[time, record], ...], option]
			for _, e := range body {
				if pair, ok := e.([]any); ok && len(pair) >= 2 {
					s.entry(tag, pair[0], pair[1], parts)
				}
			}
			option = optionAt(msg, 2)
		case []byte, string: // PackedForward: [tag, msgpack stream, option]
			option = optionAt(msg, 2)
			var raw []byte
			if b, ok := body.([]byte); ok {
				raw = b
			} else {
				raw = []byte(body.(string))
			}
			if option["compressed"] == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(raw))
				if err != nil {
					continue
				}
				raw, err = io.ReadAll(io.LimitReader(zr, maxObject))
				if err != nil {
					continue
				}
			}
			pr := &mpReader{r: bufio.NewReader(bytes.NewReader(raw))}
			for {
				e, err := pr.decode()
				if err != nil {
					break
				}
				if pair, ok := e.([]any); ok && len(pair) >= 2 {
					s.entry(tag, pair[0], pair[1], parts)
				}
			}
		default: // Message mode: [tag, time, record, option]
			if len(msg) >= 3 {
				s.entry(tag, msg[1], msg[2], parts)
			}
			option = optionAt(msg, 3)
		}
		if chunk, ok := option["chunk"].(string); ok && chunk != "" {
			w := &mpWriter{}
			w.mapHeader(1)
			w.str("ack")
			w.str(chunk)
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write(w.b); err != nil {
				return
			}
		}
	}
}

func optionAt(msg []any, i int) map[string]any {
	if len(msg) > i {
		if m, ok := msg[i].(map[string]any); ok {
			return m
		}
	}
	return nil
}

func (s *ForwardServer) entry(tag string, t any, rec any, parts map[string]*strings.Builder) {
	m, ok := rec.(map[string]any)
	if !ok {
		return
	}
	var ts time.Time
	switch v := t.(type) {
	case time.Time:
		ts = v
	case int64:
		ts = time.Unix(v, 0)
	case float64:
		ts = time.Unix(0, int64(v*1e9))
	default:
		ts = time.Now()
	}
	line := asString(m["log"])
	if id := asString(m["partial_id"]); id != "" && asString(m["partial_message"]) == "true" {
		b := parts[id]
		if b == nil {
			b = &strings.Builder{}
			parts[id] = b
		}
		if b.Len() < maxLine {
			b.WriteString(line)
		} // past the cap the rest is dropped: the stored line ends where the cap was hit
		if asString(m["partial_last"]) != "true" {
			return
		}
		line = b.String()
		delete(parts, id)
	}
	line = strings.TrimRight(line, "\r\n")
	s.Handle(tag, Record{Time: ts, Stderr: asString(m["source"]) == "stderr", Line: line})
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}
