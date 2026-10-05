package logs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// A minimal msgpack decoder: enough for the fluentd forward protocol that docker's fluentd
// log driver speaks. Maps decode to map[string]any, ext type 0 (EventTime) to time.Time.

var errTooLarge = errors.New("msgpack: object too large")

const maxObject = 16 << 20

type mpReader struct {
	r *bufio.Reader
}

func (m *mpReader) byte1() (byte, error) { return m.r.ReadByte() }

func (m *mpReader) n(k int) ([]byte, error) {
	if k > maxObject {
		return nil, errTooLarge
	}
	b := make([]byte, k)
	_, err := io.ReadFull(m.r, b)
	return b, err
}

func (m *mpReader) uint(k int) (uint64, error) {
	b, err := m.n(k)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// decode reads one value.
func (m *mpReader) decode() (any, error) {
	c, err := m.byte1()
	if err != nil {
		return nil, err
	}
	switch {
	case c <= 0x7f:
		return int64(c), nil
	case c >= 0xe0:
		return int64(int8(c)), nil
	case c&0xf0 == 0x80:
		return m.mapN(int(c & 0x0f))
	case c&0xf0 == 0x90:
		return m.arrN(int(c & 0x0f))
	case c&0xe0 == 0xa0:
		b, err := m.n(int(c & 0x1f))
		return string(b), err
	}
	switch c {
	case 0xc0:
		return nil, nil
	case 0xc2:
		return false, nil
	case 0xc3:
		return true, nil
	case 0xc4, 0xc5, 0xc6: // bin
		l, err := m.uint(1 << (c - 0xc4))
		if err != nil {
			return nil, err
		}
		return m.n(int(l))
	case 0xd9, 0xda, 0xdb: // str
		l, err := m.uint(1 << (c - 0xd9))
		if err != nil {
			return nil, err
		}
		b, err := m.n(int(l))
		return string(b), err
	case 0xcc, 0xcd, 0xce, 0xcf:
		v, err := m.uint(1 << (c - 0xcc))
		return int64(v), err
	case 0xd0, 0xd1, 0xd2, 0xd3:
		size := 1 << (c - 0xd0)
		v, err := m.uint(size)
		shift := 64 - 8*size
		return int64(v<<shift) >> shift, err
	case 0xca:
		v, err := m.uint(4)
		return float64(math.Float32frombits(uint32(v))), err
	case 0xcb:
		v, err := m.uint(8)
		return math.Float64frombits(v), err
	case 0xdc, 0xdd:
		l, err := m.uint(2 << (c - 0xdc))
		if err != nil {
			return nil, err
		}
		return m.arrN(int(l))
	case 0xde, 0xdf:
		l, err := m.uint(2 << (c - 0xde))
		if err != nil {
			return nil, err
		}
		return m.mapN(int(l))
	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8: // fixext 1..16
		return m.ext(1 << (c - 0xd4))
	case 0xc7, 0xc8, 0xc9:
		l, err := m.uint(1 << (c - 0xc7))
		if err != nil {
			return nil, err
		}
		return m.ext(int(l))
	}
	return nil, fmt.Errorf("msgpack: unknown type 0x%x", c)
}

func (m *mpReader) ext(size int) (any, error) {
	typ, err := m.byte1()
	if err != nil {
		return nil, err
	}
	b, err := m.n(size)
	if err != nil {
		return nil, err
	}
	if typ == 0 && size == 8 { // fluentd EventTime: seconds, nanoseconds (big-endian uint32)
		return time.Unix(int64(binary.BigEndian.Uint32(b[:4])), int64(binary.BigEndian.Uint32(b[4:]))), nil
	}
	return b, nil
}

func (m *mpReader) arrN(n int) ([]any, error) {
	if n > maxObject {
		return nil, errTooLarge
	}
	out := make([]any, 0, min(n, 1024))
	for i := 0; i < n; i++ {
		v, err := m.decode()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *mpReader) mapN(n int) (map[string]any, error) {
	if n > maxObject {
		return nil, errTooLarge
	}
	out := make(map[string]any, min(n, 64))
	for i := 0; i < n; i++ {
		k, err := m.decode()
		if err != nil {
			return nil, err
		}
		v, err := m.decode()
		if err != nil {
			return nil, err
		}
		switch ks := k.(type) {
		case string:
			out[ks] = v
		case []byte:
			out[string(ks)] = v
		default:
			out[fmt.Sprint(k)] = v
		}
	}
	return out, nil
}

// Encoding, used for acks and in tests.

type mpWriter struct{ b []byte }

func (w *mpWriter) str(s string) {
	switch {
	case len(s) < 32:
		w.b = append(w.b, 0xa0|byte(len(s)))
	case len(s) < 256:
		w.b = append(w.b, 0xd9, byte(len(s)))
	case len(s) < 65536:
		w.b = append(w.b, 0xda, byte(len(s)>>8), byte(len(s)))
	default:
		w.b = append(w.b, 0xdb, byte(len(s)>>24), byte(len(s)>>16), byte(len(s)>>8), byte(len(s)))
	}
	w.b = append(w.b, s...)
}

func (w *mpWriter) mapHeader(n int) { w.b = append(w.b, 0x80|byte(n)) }
func (w *mpWriter) arrHeader(n int) { w.b = append(w.b, 0x90|byte(n)) }

func (w *mpWriter) eventTime(t time.Time) {
	w.b = append(w.b, 0xd7, 0)
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(t.Unix()))
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(t.Nanosecond()))
}

func (w *mpWriter) int(v int64) {
	w.b = append(w.b, 0xd3)
	w.b = binary.BigEndian.AppendUint64(w.b, uint64(v))
}
