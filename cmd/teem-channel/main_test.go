//go:build e2e
// +build e2e

// Run with: go test -tags=e2e ./cmd/teem-channel/...
//
// This file is gated behind the e2e build tag because it shells out to
// `go build` (~1s) and spawns the shim as a subprocess. The default
// suite skips it; CI runs it explicitly.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStdioShim_EndToEnd is the wire-format proof for the
// SSE → stdio channel-notification path. It:
//
//  1. Stands up a fake daemon that serves one canned SSE event under
//     /teams/<name>/channel-events.
//  2. Builds and spawns the teem-channel binary as a subprocess,
//     pointing it at the fake daemon.
//  3. Drives the MCP initialize handshake over the subprocess's stdio
//     and asserts experimental.claude/channel is declared.
//  4. Reads the next stdio message and asserts it's a
//     notifications/claude/channel matching the canned event.
//
// Failure of this test is a regression of the contract Claude Code's
// channel listener consumes.
func TestStdioShim_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end shim test is slow")
	}

	// Build the shim under the test's temp dir so we don't pollute the
	// workspace and don't depend on `go install`.
	bin := filepath.Join(t.TempDir(), "teem-channel")
	buildCmd := exec.Command("go", "build", "-o", bin, ".")
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, buildOut)
	}

	// Fake daemon: one client per test, one event served, then we
	// just hold the connection open until the client drops.
	const wantContent = "worker-ada finished job j-1234"
	wantMeta := map[string]string{
		"agent_id": "worker-ada",
		"job_id":   "j-1234",
		"kind":     "job_complete",
	}

	connected := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/teams/example/channel-events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		select {
		case connected <- struct{}{}:
		default:
		}
		body, _ := json.Marshal(map[string]any{
			"content": wantContent,
			"meta":    wantMeta,
		})
		fmt.Fprintf(w, "event: channel\ndata: %s\n\n", body)
		fl.Flush()
		// Hold open until the client (the shim) drops or context dies.
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--team", "example", "--endpoint", server.URL)
	cmd.Env = append(os.Environ(), "TEEM_WORKER_TOKEN=test-token")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	var stderrMu sync.Mutex
	cmd.Stderr = &lockedWriter{w: &stderr, mu: &stderrMu}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stderrMu.Lock()
		s := stderr.String()
		stderrMu.Unlock()
		if s != "" {
			t.Logf("teem-channel stderr:\n%s", s)
		}
	})

	reader := bufio.NewReader(stdout)

	// 1. initialize handshake.
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n"
	if _, err := stdin.Write([]byte(initReq)); err != nil {
		t.Fatalf("write init: %v", err)
	}
	initLine, err := readLineWithTimeout(reader, 10*time.Second)
	if err != nil {
		t.Fatalf("read init resp: %v", err)
	}
	var initResp struct {
		Result struct {
			Capabilities struct {
				Experimental map[string]any `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(initLine), &initResp); err != nil {
		t.Fatalf("unmarshal init: %v\nraw: %s", err, initLine)
	}
	if _, ok := initResp.Result.Capabilities.Experimental["claude/channel"]; !ok {
		t.Fatalf("initialize did not declare experimental.claude/channel; resp: %s", initLine)
	}

	// 2. notifications/initialized so the server marks the session live.
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatalf("write initialized: %v", err)
	}

	// 3. Wait for the shim to dial the SSE endpoint, then read the
	//    forwarded notification.
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("shim did not connect to SSE endpoint within 5s")
	}

	// Drain stdout until we see the channel notification. The
	// notifications/initialized has no response, so the next line out
	// should be our forwarded notification — but be lenient and skip
	// non-matching frames in case any other noise is emitted.
	deadline := time.Now().Add(10 * time.Second)
	var got struct {
		Method string `json:"method"`
		Params struct {
			Content string            `json:"content"`
			Meta    map[string]string `json:"meta"`
		} `json:"params"`
	}
	for time.Now().Before(deadline) {
		line, err := readLineWithTimeout(reader, time.Until(deadline))
		if err != nil {
			t.Fatalf("read notification: %v", err)
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			continue
		}
		if got.Method == "notifications/claude/channel" {
			break
		}
		got.Method = ""
	}
	if got.Method != "notifications/claude/channel" {
		t.Fatal("did not receive notifications/claude/channel within deadline")
	}
	if got.Params.Content != wantContent {
		t.Fatalf("content = %q, want %q", got.Params.Content, wantContent)
	}
	for k, v := range wantMeta {
		if got.Params.Meta[k] != v {
			t.Fatalf("meta[%q] = %q, want %q", k, got.Params.Meta[k], v)
		}
	}
}

// readLineWithTimeout reads one newline-terminated line, failing if
// it takes longer than d. Used to make the test fail fast instead of
// hanging on the global test timeout.
func readLineWithTimeout(r *bufio.Reader, d time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line: line, err: err}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-time.After(d):
		return "", fmt.Errorf("read timed out after %s", d)
	}
}

type lockedWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
