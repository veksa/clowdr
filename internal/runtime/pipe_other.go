//go:build !windows

package runtime

import (
	"context"
	"errors"
	"net"
)

const defaultHost = "unix:///var/run/docker.sock"

func dialPipe(context.Context, string) (net.Conn, error) {
	return nil, errors.New("npipe:// is only available on Windows")
}
