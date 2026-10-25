package monitor

import (
	"os"
	"syscall"
)

// DiskFree reports free (for unprivileged users) and total bytes of the filesystem at path.
func DiskFree(path string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * st.Bsize, int64(st.Blocks) * st.Bsize, true
}

func meminfo() map[string]int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseMeminfo(f)
}

func memAvailable() (avail, total int64, ok bool) {
	m := meminfo()
	return m["MemAvailable"], m["MemTotal"], m["MemTotal"] > 0
}

// swapFree is the host's swap; total is 0 when there is none. Containers never get any
// (MemorySwap equals the limit), so whatever is in use was pushed out of the host's own processes
// and of memory clowdr did not promise.
func swapFree() (free, total int64, ok bool) {
	m := meminfo()
	return m["SwapFree"], m["SwapTotal"], m != nil
}
