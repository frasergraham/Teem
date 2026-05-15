package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/pulse"
)

// chatRequest is the JSON body posted by the dashboard's chat panel.
type chatRequest struct {
	Message string `json:"message"`
}

// chatRunner is the subprocess seam: production wires it to a real
// `claude -p` invocation; tests inject a fake that emits canned
// stream-json on stdout. Returns the spawned process's stdout reader
// and a wait callback that blocks until the subprocess exits.
type chatRunner func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error)

// defaultChatRunner is the production runner: locates `claude` on PATH
// and spawns it with the chat-flavour argv from pulse.BuildChatArgs.
func defaultChatRunner(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, nil, fmt.Errorf("claude CLI not on PATH: %w", err)
	}
	args := pulse.BuildChatArgs(mcpConfig, contextBody, userMessage)
	cmd := exec.CommandContext(ctx, claudePath, args...)
	cmd.Dir = repoRoot
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start claude: %w", err)
	}
	return stdout, cmd.Wait, nil
}

// handleChatTeam serves POST /control/teams/<id>/chat — the dashboard's
// direct-chat panel. Each request spawns a one-shot leader subprocess
// (no session retention) and streams the assistant text to the browser
// as Server-Sent Events.
//
// Auth: localhost / tailnet boundary, same as /ping and the dashboard
// itself. Per-user auth is not yet wired.
func (d *daemon) handleChatTeam(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/control/teams/")
	key := strings.TrimSuffix(rest, "/chat")
	if key == "" || strings.ContainsRune(key, '/') {
		http.Error(w, "bad team id", http.StatusBadRequest)
		return
	}
	rt := d.resolveTeam(key)
	if rt == nil {
		http.NotFound(w, r)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	runner := d.chatRunner
	if runner == nil {
		runner = defaultChatRunner
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	mcpConfig := filepath.Join(defaultStateDir(rt.team.ID), "pulse-mcp.json")
	contextBody := fmt.Sprintf(
		"You are responding to a direct chat message from the operator on the dashboard.\n"+
			"Take one turn — be concise. Use list_tasks / list_agents / query_audit if you need state.\n"+
			"Sent at: %s\n",
		time.Now().UTC().Format(time.RFC3339),
	)

	ctx := r.Context()
	stdout, wait, err := runner(ctx, mcpConfig, rt.repoRoot, contextBody, msg)
	if err != nil {
		writeSSE(w, flusher, "error", err.Error())
		return
	}

	parseErr := streamChatResponse(stdout, w, flusher)
	if waitErr := wait(); waitErr != nil && parseErr == nil {
		writeSSE(w, flusher, "error", waitErr.Error())
		return
	}
	if parseErr != nil {
		writeSSE(w, flusher, "error", parseErr.Error())
		return
	}
	writeSSE(w, flusher, "done", "")
}

// streamChatResponse parses Claude Code's stream-json output and
// forwards each assistant text chunk to the SSE writer as a default
// (unnamed) data event. Tool-use blocks and result events are dropped
// so the chat panel only sees user-visible prose.
func streamChatResponse(r io.Reader, w http.ResponseWriter, f http.Flusher) error {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type assistantMsg struct {
		Content []contentBlock `json:"content"`
	}
	type ev struct {
		Type    string       `json:"type"`
		Result  string       `json:"result"`
		Message assistantMsg `json:"message"`
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var emittedAssistant bool
	var resultText string
	for sc.Scan() {
		line := sc.Bytes()
		var e ev
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			for _, c := range e.Message.Content {
				if c.Type == "text" && c.Text != "" {
					writeSSE(w, f, "", c.Text)
					emittedAssistant = true
				}
			}
		case "result":
			if e.Result != "" {
				resultText = e.Result
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	// Fall back to the result text if no assistant text frames came
	// through (older claude builds, or a tool-only turn).
	if !emittedAssistant && resultText != "" {
		writeSSE(w, f, "", resultText)
	}
	return nil
}

// writeSSE writes one Server-Sent Events frame. Empty event names are
// the default ("message") channel; the chat panel listens for those
// plus "done" / "error". Multi-line bodies are split into one `data:`
// line per source line per the SSE spec.
func writeSSE(w http.ResponseWriter, f http.Flusher, event, body string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	for _, line := range strings.Split(body, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	if f != nil {
		f.Flush()
	}
}
