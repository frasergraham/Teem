// teem-channel is a stdio MCP shim that bridges the daemon's per-team
// channel-event SSE stream into a Claude Code subprocess. Claude Code
// only fires channel listeners on stdio MCP servers it spawned
// itself, so the daemon's HTTP MCP server (where the tools live)
// can't directly wake the leader on a worker event. This shim is the
// stdio half of that path:
//
//  1. Claude Code launches this binary as a subprocess (see the
//     "teem-channel" entry in claude-mcp.json / pulse-mcp.json).
//  2. We declare experimental.claude/channel in our initialize
//     response, so claude registers as a listener.
//  3. We open a long-lived GET against
//     <endpoint>/teams/<team>/channel-events (SSE, bearer-auth via
//     ~/.teem/worker_token) and re-emit every received Event as a
//     notifications/claude/channel message on stdio.
//
// The shim owns no state. Restarting it is safe; the daemon's SSE
// handler will accept the reconnect and start fanning fresh events.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

const channelInstructions = "Events from teem arrive as <channel source=\"teem\" kind=\"...\" agent_id=\"...\"> blocks. " +
	"Read and act — these are wake events for worker job lifecycle (complete/error/interrupted/stopped), " +
	"task stage changes, and decision/blocker notes from your team. They are nudges to take a turn; " +
	"call list_tasks, query_audit, or get_results on the teem MCP server to follow up."

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "teem-channel:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("teem-channel", flag.ContinueOnError)
	team := fs.String("team", "", "team name (required)")
	endpoint := fs.String("endpoint", "", "daemon endpoint, e.g. http://127.0.0.1:7777 (default: ~/.teem/daemon.json)")
	tokenPath := fs.String("token-file", "", "path to the worker token file (default: ~/.teem/worker_token)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *team == "" {
		return errors.New("--team is required")
	}
	ep := strings.TrimRight(*endpoint, "/")
	if ep == "" {
		fromDaemon, err := readDaemonEndpoint()
		if err != nil {
			return fmt.Errorf("resolve endpoint: %w", err)
		}
		ep = strings.TrimRight(fromDaemon, "/")
	}
	if ep == "" {
		return errors.New("no daemon endpoint (pass --endpoint or run `teem start` first)")
	}
	token, err := readWorkerToken(*tokenPath)
	if err != nil {
		return fmt.Errorf("read worker token: %w", err)
	}

	core := mcpsrv.NewMCPServer(
		"teem-channel",
		"0.1.0",
		mcpsrv.WithExperimental(map[string]any{
			"claude/channel": map[string]any{},
		}),
		mcpsrv.WithInstructions(channelInstructions),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SSE → stdio forwarder. Reconnect with backoff if the daemon
	// drops the connection or restarts; the daemon is the source of
	// truth, the shim is stateless.
	go forwardSSE(ctx, fmt.Sprintf("%s/teams/%s/channel-events", ep, *team), token, core)

	return mcpsrv.ServeStdio(core)
}

// errAuthRejected is returned by streamOnce when the daemon answers
// 401 or 403. forwardSSE treats this as fatal (no retry) and exits the
// process so Claude Code surfaces the failure instead of the shim
// silently spinning forever.
var errAuthRejected = errors.New("auth rejected by daemon")

// forwardSSE holds open one GET against the daemon's channel-events
// SSE stream and re-emits every received Event as a stdio
// notifications/claude/channel message. On a retriable error or EOF it
// sleeps with exponential backoff and reconnects; on auth rejection it
// exits the process. A stream that stayed up for >60s before returning
// resets the backoff so a single flap after hours of uptime doesn't
// leave us at the cap.
func forwardSSE(ctx context.Context, url, token string, core *mcpsrv.MCPServer) {
	const initialBackoff = 500 * time.Millisecond
	const maxBackoff = 30 * time.Second
	const stableThreshold = 60 * time.Second
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		start := time.Now()
		err := streamOnce(ctx, url, token, core)
		uptime := time.Since(start)
		if errors.Is(err, errAuthRejected) {
			fmt.Fprintf(os.Stderr, "[teem-channel] auth rejected by daemon: %v — refresh ~/.teem/worker_token or restart teem\n", err)
			os.Exit(1)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "teem-channel: sse stream: %v (retrying in %s)\n", err, backoff)
		}
		if uptime > stableThreshold {
			backoff = initialBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// streamOnce dials the SSE endpoint, parses event/data frames, and
// forwards each Event as a stdio notification. Returns nil on a clean
// remote-side close so forwardSSE reconnects on the standard cadence.
// Returns errAuthRejected for 401/403 so forwardSSE can fail loud.
func streamOnce(ctx context.Context, url, token string, core *mcpsrv.MCPServer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%w: %s %s", errAuthRejected, resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	reader := bufio.NewReader(resp.Body)
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// dispatch the accumulated event
			if event == "channel" && data != "" {
				dispatch(core, data)
			}
			event, data = "", ""
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			// SSE comment / keepalive — ignore.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(chunk, " ") {
				chunk = chunk[1:]
			}
			if data == "" {
				data = chunk
			} else {
				data = data + "\n" + chunk
			}
		}
	}
}

// dispatch parses a single SSE data payload (JSON {content, meta})
// and re-emits it as notifications/claude/channel on every connected
// stdio session. There's exactly one stdio session (claude) for the
// lifetime of this process.
func dispatch(core *mcpsrv.MCPServer, raw string) {
	var ev struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		fmt.Fprintf(os.Stderr, "teem-channel: bad SSE payload: %v\n", err)
		return
	}
	params := map[string]any{"content": ev.Content}
	if len(ev.Meta) > 0 {
		m := make(map[string]any, len(ev.Meta))
		for k, v := range ev.Meta {
			m[k] = v
		}
		params["meta"] = m
	}
	core.SendNotificationToAllClients("notifications/claude/channel", params)
}

// readDaemonEndpoint reads ~/.teem/daemon.json and returns the
// recorded endpoint. Used when --endpoint isn't passed.
func readDaemonEndpoint() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(filepath.Join(home, ".teem", "daemon.json"))
	if err != nil {
		return "", err
	}
	var s struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", err
	}
	return s.Endpoint, nil
}

// readWorkerToken returns the bearer token used to authenticate the
// SSE connection. Resolution order: explicit --token-file, then
// $TEEM_WORKER_TOKEN, then ~/.teem/worker_token. An empty token is
// allowed (sends no Authorization header) so the shim is usable in
// tests against an unauth fake daemon.
func readWorkerToken(explicit string) (string, error) {
	if v := os.Getenv("TEEM_WORKER_TOKEN"); v != "" {
		return v, nil
	}
	path := explicit
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".teem", "worker_token")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// Compile-time check we're using mcpgo. The package is referenced via
// WithExperimental's map keys (strings), but importing it explicitly
// keeps the dependency declared and future-proofs a stricter typed
// option API.
var _ = mcpgo.LATEST_PROTOCOL_VERSION
