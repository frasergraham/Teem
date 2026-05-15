package executor

import (
	"context"
	"net"
	"net/http"
	"time"
)

// NewUnixClient returns an *http.Client whose Transport dials a unix
// socket at socketPath. The URL host is ignored (we use a constant
// "unix" in the executor's BaseURL) so callers don't need to encode
// anything socket-specific into requests.
//
// Used by the local-worker model where teem-worker listens on a unix
// socket per agent under ~/.teem/sockets/<team>/<id>.sock. Identical
// HTTPExecutor code path as the tailnet variant; only the dialer
// differs.
func NewUnixClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 0, // long-polls need to outlive any default
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
			},
			// Long-poll friendly: keep connections warm.
			MaxIdleConns:       4,
			IdleConnTimeout:    90 * time.Second,
			DisableCompression: true,
		},
	}
}
