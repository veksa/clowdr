package monitor

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
)

// followKernelOOM reads /dev/kmsg from its end, one record per read, and reports the
// containers of OOM kills until ctx ends. It needs root (CAP_SYSLOG with dmesg_restrict).
func followKernelOOM(ctx context.Context, found func(id string)) error {
	f, err := os.Open("/dev/kmsg")
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()
	buf := make([]byte, 8192)
	for {
		n, err := f.Read(buf)
		switch {
		case errors.Is(err, syscall.EPIPE):
			continue // records were overwritten before they were read; go on with the next
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, id := range oomKilled(string(buf[:n])) {
			found(id)
		}
	}
}
