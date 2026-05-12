// Package tailnet wraps tailscale.com/tsnet so Teem can join the user's
// tailnet without requiring a system-wide tailscaled install.
//
// The Node owns a tsnet.Server and exposes the minimum surface the rest
// of Teem needs: a tailnet-scoped net.Listener for the MCP server, an
// HTTP client that dials peers by tailnet hostname, and accessors for the
// node's own hostname (so worker MCP configs can point at the Leader).
package tailnet
