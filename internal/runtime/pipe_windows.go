package runtime

import (
	"context"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

const defaultHost = "npipe:////./pipe/docker_engine"

// dialPipe connects to Docker Desktop's named pipe: npipe:////./pipe/docker_engine.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, strings.ReplaceAll(path, "/", `\`))
}
