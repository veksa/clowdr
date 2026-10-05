//go:build !linux

package monitor

func diskFree(string) (free, total int64, ok bool) { return 0, 0, false }

func memAvailable() (avail, total int64, ok bool) { return 0, 0, false }
