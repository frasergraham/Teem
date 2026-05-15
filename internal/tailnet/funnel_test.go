package tailnet

import (
	"errors"
	"net"
	"strconv"
	"testing"

	"tailscale.com/ipn"
)

func TestBuildFunnelServeConfig(t *testing.T) {
	const fqdn = "teem.tail-scale.ts.net"
	const path = "/messaging/telegram/webhook"
	const localPort = 7778

	sc := buildFunnelServeConfig(fqdn, FunnelHTTPSPort, path, localPort)

	if sc == nil {
		t.Fatal("nil serve config")
	}

	// TCP[443] should declare HTTPS terminating at the node.
	tcp, ok := sc.TCP[FunnelHTTPSPort]
	if !ok {
		t.Fatalf("TCP[%d] missing in %+v", FunnelHTTPSPort, sc.TCP)
	}
	if !tcp.HTTPS || tcp.HTTP {
		t.Fatalf("TCP[%d] = %+v, want HTTPS=true HTTP=false", FunnelHTTPSPort, tcp)
	}

	// Web[fqdn:443] should hold the proxy handler at the requested mount.
	hp := ipn.HostPort(net.JoinHostPort(fqdn, strconv.Itoa(int(FunnelHTTPSPort))))
	webCfg, ok := sc.Web[hp]
	if !ok {
		t.Fatalf("Web[%q] missing; got keys=%v", hp, sc.Web)
	}
	h, ok := webCfg.Handlers[path]
	if !ok {
		t.Fatalf("Web[%q].Handlers[%q] missing; got %+v", hp, path, webCfg.Handlers)
	}
	wantProxy := "http://127.0.0.1:" + strconv.Itoa(localPort)
	if h.Proxy != wantProxy {
		t.Fatalf("Proxy = %q, want %q", h.Proxy, wantProxy)
	}

	// AllowFunnel[fqdn:443] must be true.
	if !sc.AllowFunnel[hp] {
		t.Fatalf("AllowFunnel[%q] not set; got %+v", hp, sc.AllowFunnel)
	}
}

func TestIsFunnelDeniedErr(t *testing.T) {
	yes := []error{
		errors.New("Funnel not available; HTTPS must be enabled."),
		errors.New("funnel is not enabled for this node"),
		errors.New("you are not allowed to use Funnel on port 443"),
		errors.New("Funnel access denied"),
	}
	for _, e := range yes {
		if !isFunnelDeniedErr(e) {
			t.Errorf("expected funnel-denied for %q", e)
		}
	}
	no := []error{
		nil,
		errors.New("connection refused"),
		errors.New("listener already in use"),
	}
	for _, e := range no {
		if isFunnelDeniedErr(e) {
			t.Errorf("expected NOT funnel-denied for %v", e)
		}
	}
}
