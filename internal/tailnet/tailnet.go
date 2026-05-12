package tailnet

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"tailscale.com/tsnet"
)

// Config configures the tailnet node.
type Config struct {
	// Hostname is the name the node presents on the tailnet (DNS label).
	Hostname string
	// AuthKey is the tailnet auth key. If empty, tsnet falls back to the
	// TS_AUTHKEY env var and, failing that, prints a login URL.
	AuthKey string
	// StateDir is where tsnet persists its state. Defaults to
	// ~/.teem/tsnet/<hostname>.
	StateDir string
	// Logf, if non-nil, receives verbose backend logs. Defaults to
	// discarding them.
	Logf func(format string, args ...any)
	// UserLogf, if non-nil, receives user-facing messages (auth URL, etc).
	// Defaults to stderr.
	UserLogf func(format string, args ...any)
	// Ephemeral nodes don't persist in the tailnet between runs.
	Ephemeral bool
}

// Node is a running tailnet node.
type Node struct {
	srv      *tsnet.Server
	hostname string
}

// New returns a Node configured but not yet running. Call Start.
func New(cfg Config) (*Node, error) {
	if cfg.Hostname == "" {
		return nil, fmt.Errorf("tailnet: hostname is required")
	}
	if !validHostname(cfg.Hostname) {
		return nil, fmt.Errorf("tailnet: hostname %q is not a valid DNS label", cfg.Hostname)
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("tailnet: resolve home: %w", err)
		}
		stateDir = filepath.Join(home, ".teem", "tsnet", cfg.Hostname)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tailnet: state dir: %w", err)
	}
	srv := &tsnet.Server{
		Hostname:  cfg.Hostname,
		Dir:       stateDir,
		AuthKey:   cfg.AuthKey,
		Ephemeral: cfg.Ephemeral,
	}
	if cfg.Logf != nil {
		srv.Logf = cfg.Logf
	} else {
		srv.Logf = func(string, ...any) {}
	}
	if cfg.UserLogf != nil {
		srv.UserLogf = cfg.UserLogf
	} else {
		srv.UserLogf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "tailnet: "+format+"\n", args...)
		}
	}
	return &Node{srv: srv, hostname: cfg.Hostname}, nil
}

// Start brings the node up, blocking until it has joined the tailnet or
// ctx is cancelled.
func (n *Node) Start(ctx context.Context) error {
	if _, err := n.srv.Up(ctx); err != nil {
		return fmt.Errorf("tailnet: bring up: %w", err)
	}
	return nil
}

// Listen returns a net.Listener bound on the tailnet interface.
func (n *Node) Listen(network, addr string) (net.Listener, error) {
	return n.srv.Listen(network, addr)
}

// HTTPClient returns an http.Client whose dialer resolves tailnet
// hostnames.
func (n *Node) HTTPClient() *http.Client {
	return n.srv.HTTPClient()
}

// Hostname returns the node's tailnet hostname (the unqualified label).
func (n *Node) Hostname() string {
	return n.hostname
}

// Addrs returns the node's tailnet IP addresses. Both v4 and v6 if
// available.
func (n *Node) Addrs() []netip.Addr {
	v4, v6 := n.srv.TailscaleIPs()
	var out []netip.Addr
	if v4.IsValid() {
		out = append(out, v4)
	}
	if v6.IsValid() {
		out = append(out, v6)
	}
	return out
}

// Close shuts the node down and logs it out (if Ephemeral was true).
func (n *Node) Close() error {
	if n == nil || n.srv == nil {
		return nil
	}
	return n.srv.Close()
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostname(s string) bool {
	return hostnameRE.MatchString(strings.ToLower(s))
}
