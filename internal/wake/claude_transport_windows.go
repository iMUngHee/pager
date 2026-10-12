package wake

import (
	"context"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

func dialClaude(ctx context.Context, path string) (net.Conn, error) {
	// Claude Code on Windows publishes a named pipe, not an AF_UNIX socket.
	if strings.HasPrefix(strings.ToLower(path), `\\.\pipe\`) {
		return winio.DialPipeContext(ctx, path)
	}
	// Preserve AF_UNIX support for hosts and tests that use Unix sockets.
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", path)
}
