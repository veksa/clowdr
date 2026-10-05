package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that unmarshals from YAML strings like "30s", "24h" or "14d".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// ParseDuration extends time.ParseDuration with a "d" (day) suffix.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}

// Bytes is a size that unmarshals from "450m", "1g", "200MB", "8m" or a plain number of bytes.
// Suffixes are binary (k = 1024), as in `docker run --memory`.
type Bytes int64

func (b *Bytes) UnmarshalText(t []byte) error {
	v, err := ParseBytes(string(t))
	if err != nil {
		return err
	}
	*b = Bytes(v)
	return nil
}

func (b Bytes) MarshalText() ([]byte, error) { return []byte(FormatBytes(int64(b))), nil }

func ParseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "b")
	s = strings.TrimSuffix(s, "i")
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult = 1 << 10
	case strings.HasSuffix(s, "m"):
		mult = 1 << 20
	case strings.HasSuffix(s, "g"):
		mult = 1 << 30
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

func FormatBytes(n int64) string {
	switch {
	case n == 0:
		return "0"
	case n%(1<<30) == 0:
		return strconv.FormatInt(n>>30, 10) + "g"
	case n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + "m"
	case n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}
