package tailnet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"tailscale.com/ipn"
)

// FunnelHTTPSPort is the public-facing HTTPS port the daemon currently
// uses for Funnel. tsnet only supports 443/8443/10000 for Funnel; 443 is
// the natural fit since the operator-supplied PublicURL has no port
// suffix.
const FunnelHTTPSPort uint16 = 443

// EnableFunnel configures the tsnet node so that requests to
// https://<node-fqdn>/<path> arriving via Tailscale Funnel are proxied
// to http://127.0.0.1:<localPort>. The tailnet ACL must allow Funnel
// for this node; if not, EnableFunnel returns a wrapped error telling
// the operator how to fix it. Returns the resolved fqdn on success so
// callers can log / surface it without a second LocalClient round-trip.
//
// The webhook listener must be bound on 127.0.0.1 — tsnet's serve
// daemon proxies from the funnel-terminated HTTPS endpoint into the
// host loopback, not onto the tsnet node's own listener.
func (n *Node) EnableFunnel(ctx context.Context, path string, localPort int) (string, error) {
	if n == nil || n.srv == nil {
		return "", errors.New("tailnet: node not initialised")
	}
	if path == "" || path[0] != '/' {
		return "", fmt.Errorf("tailnet: funnel path %q must start with /", path)
	}
	if localPort <= 0 || localPort > 65535 {
		return "", fmt.Errorf("tailnet: funnel local port %d out of range", localPort)
	}
	fqdn, err := n.fqdn(ctx)
	if err != nil {
		return "", fmt.Errorf("tailnet: funnel resolve fqdn: %w", err)
	}
	sc := buildFunnelServeConfig(fqdn, FunnelHTTPSPort, path, localPort)
	lc, err := n.srv.LocalClient()
	if err != nil {
		return "", fmt.Errorf("tailnet: funnel local client: %w", err)
	}
	if err := lc.SetServeConfig(ctx, sc); err != nil {
		if isFunnelDeniedErr(err) {
			return "", fmt.Errorf("funnel via tsnet: enable Funnel on node %q in the tailnet admin UI (https://login.tailscale.com/admin/acls), then bounce daemon: %w", fqdn, err)
		}
		return "", fmt.Errorf("tailnet: SetServeConfig: %w", err)
	}
	return fqdn, nil
}

// DisableFunnel clears the tsnet node's serve config. Safe to call on a
// node that never had a serve config set. Used for graceful shutdown so
// Telegram retries don't hit a serve config that points at a
// soon-to-be-dead webhook listener.
func (n *Node) DisableFunnel(ctx context.Context) error {
	if n == nil || n.srv == nil {
		return nil
	}
	lc, err := n.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("tailnet: funnel local client: %w", err)
	}
	if err := lc.SetServeConfig(ctx, &ipn.ServeConfig{}); err != nil {
		return fmt.Errorf("tailnet: clear serve config: %w", err)
	}
	return nil
}

// FQDN returns the tsnet node's fully-qualified DNS name with the
// trailing dot stripped, e.g. "teem.tail-scale.ts.net". Returns an
// error if the node isn't up yet.
func (n *Node) FQDN(ctx context.Context) (string, error) {
	if n == nil || n.srv == nil {
		return "", errors.New("tailnet: node not initialised")
	}
	return n.fqdn(ctx)
}

func (n *Node) fqdn(ctx context.Context) (string, error) {
	if cd := n.srv.CertDomains(); len(cd) > 0 && cd[0] != "" {
		return strings.TrimSuffix(cd[0], "."), nil
	}
	lc, err := n.srv.LocalClient()
	if err != nil {
		return "", err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return "", err
	}
	if st == nil || st.Self == nil || st.Self.DNSName == "" {
		return "", errors.New("tailnet: status has no Self.DNSName")
	}
	return strings.TrimSuffix(st.Self.DNSName, "."), nil
}

// buildFunnelServeConfig assembles the ipn.ServeConfig declaring:
//   - https on port https on the given fqdn,
//   - a single web handler at `path` that proxies to 127.0.0.1:localPort,
//   - AllowFunnel for that host:port.
//
// Pure function — extracted for unit-testing the shape without needing
// a running tsnet node.
func buildFunnelServeConfig(fqdn string, httpsPort uint16, path string, localPort int) *ipn.ServeConfig {
	sc := &ipn.ServeConfig{}
	// Include `path` in the proxy target so Tailscale Serve doesn't strip
	// the mount path when forwarding. Without this, the backend listener
	// sees a request for "/" and our exact-path handler 404s.
	handler := &ipn.HTTPHandler{
		Proxy: "http://127.0.0.1:" + strconv.Itoa(localPort) + path,
	}
	// Magic-DNS suffix is only consulted for service-handler hosts; for
	// our plain-FQDN setup it's unused.
	sc.SetWebHandler(handler, fqdn, httpsPort, path, true, "")
	sc.SetFunnel(fqdn, httpsPort, true)
	return sc
}

func isFunnelDeniedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"funnel not available", "funnel is not enabled", "not allowed to use funnel", "funnel access"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
