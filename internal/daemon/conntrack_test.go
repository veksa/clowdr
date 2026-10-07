package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func fakeConntrack(t *testing.T, max, buckets string) (maxPath, hashPath string) {
	t.Helper()
	dir := t.TempDir()
	oldProc, oldSys := procRoot, sysRoot
	procRoot, sysRoot = filepath.Join(dir, "proc"), filepath.Join(dir, "sys")
	t.Cleanup(func() { procRoot, sysRoot = oldProc, oldSys })
	maxPath = filepath.Join(procRoot, "sys/net/netfilter/nf_conntrack_max")
	hashPath = filepath.Join(sysRoot, "module/nf_conntrack/parameters/hashsize")
	for p, v := range map[string]string{maxPath: max, hashPath: buckets} {
		if v == "" {
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return maxPath, hashPath
}

func readFile(t *testing.T, p string) int {
	t.Helper()
	n, err := readInt(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConntrackTarget(t *testing.T) {
	for cpus, want := range map[int]int{1: 131072, 4: 131072, 8: 262144} {
		if got := conntrackTarget(cpus); got != want {
			t.Errorf("%d cpus: %d, want %d", cpus, got, want)
		}
	}
}

func TestTuneConntrackRaises(t *testing.T) {
	maxPath, hashPath := fakeConntrack(t, "8192", "8192")
	tuneConntrack(quiet)
	want := conntrackTarget(runtime.NumCPU())
	if got := readFile(t, maxPath); got != want {
		t.Errorf("nf_conntrack_max = %d, want %d", got, want)
	}
	if got := readFile(t, hashPath); got != want/4 {
		t.Errorf("hashsize = %d, want %d", got, want/4)
	}
}

func TestTuneConntrackNeverLowers(t *testing.T) {
	big := strconv.Itoa(conntrackTarget(runtime.NumCPU()) * 2)
	maxPath, hashPath := fakeConntrack(t, big, "1024")
	tuneConntrack(quiet)
	if got := strconv.Itoa(readFile(t, maxPath)); got != big {
		t.Errorf("nf_conntrack_max = %s, want %s", got, big)
	}
	if got := readFile(t, hashPath); got != 1024 {
		t.Errorf("hashsize changed to %d although the max was high enough", got)
	}
}

func TestTuneConntrackWithoutModule(t *testing.T) {
	maxPath, _ := fakeConntrack(t, "", "")
	tuneConntrack(quiet) // nothing to tune, no panic, nothing created
	if _, err := os.Stat(maxPath); !os.IsNotExist(err) {
		t.Errorf("created %s: %v", maxPath, err)
	}
}
