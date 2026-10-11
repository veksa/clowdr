package monitor

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parseMeminfo reads /proc/meminfo into bytes by field name ("MemTotal", "SwapFree", ...).
func parseMeminfo(r io.Reader) map[string]int64 {
	m := map[string]int64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		m[strings.TrimSuffix(fields[0], ":")] = v << 10
	}
	return m
}
