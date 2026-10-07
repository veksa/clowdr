package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Every request through clowdr takes two or three conntrack entries (docker-proxy or DNAT to
// nginx, nginx to the service over the bridge), and each stays 120s in TIME_WAIT. The kernel
// sizes the table from RAM: 8192 entries on a 1 GB host, which fills at ~25 new connections
// a second, after which new connections are silently dropped ("nf_conntrack: table full").
// Raise it the way kube-proxy does: 32768 per CPU, at least 131072; never lower it.
const (
	conntrackPerCPU = 32768
	conntrackMin    = 131072
)

// procRoot and sysRoot are swapped in tests.
var (
	procRoot = "/proc"
	sysRoot  = "/sys"
)

func conntrackTarget(cpus int) int {
	return max(conntrackPerCPU*cpus, conntrackMin)
}

// tuneConntrack raises nf_conntrack_max and the hash table size. Docker has loaded
// nf_conntrack by the time clowdrd starts; without it (or without permission) it only logs.
func tuneConntrack(log *slog.Logger) {
	maxPath := filepath.Join(procRoot, "sys/net/netfilter/nf_conntrack_max")
	cur, err := readInt(maxPath)
	if err != nil {
		log.Debug("conntrack: not available", "err", err)
		return
	}
	want := conntrackTarget(runtime.NumCPU())
	if cur >= want {
		return
	}
	// The hash table first: a bigger table over too few buckets makes lookups slow.
	hashPath := filepath.Join(sysRoot, "module/nf_conntrack/parameters/hashsize")
	if buckets, err := readInt(hashPath); err == nil && buckets < want/4 {
		if err := os.WriteFile(hashPath, []byte(strconv.Itoa(want/4)), 0o644); err != nil {
			log.Warn("conntrack: could not raise hashsize", "path", hashPath, "err", err)
		}
	}
	if err := os.WriteFile(maxPath, []byte(strconv.Itoa(want)), 0o644); err != nil {
		log.Warn("conntrack: could not raise nf_conntrack_max; under load new connections are dropped", "current", cur, "want", want, "err", err)
		return
	}
	log.Info("conntrack: raised nf_conntrack_max", "from", cur, "to", want)
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
