//go:build !windows

package wake

import (
	"context"
	"net"
)

func dialClaude(ctx context.Context, path string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", path)
}
