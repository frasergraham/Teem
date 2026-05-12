package leader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/frasergraham/teem/internal/transport"
)

// ClaudeLeader runs `claude -p --input-format stream-json
// --output-format stream-json` as the Leader. It is transport-pluggable —
// pass LocalTransport for local, SSHTransport for remote.
type ClaudeLeader struct {
	cfg     Config
	proc    transport.Process
	stdin   io.WriteCloser
	events  chan AssistantEvent
	closeMu sync.Mutex
	closed  bool
}

// Start launches the Leader subprocess and wires up its IO.
func Start(ctx context.Context, cfg Config) (*ClaudeLeader, error) {
	if cfg.Transport == nil {
		return nil, fmt.Errorf("leader: Transport is required")
	}
	if cfg.MCPEndpoint == "" {
		return nil, fmt.Errorf("leader: MCPEndpoint is required")
	}

	mcpPath, err := writeLocalMCPConfig(cfg.MCPEndpoint)
	if err != nil {
		return nil, fmt.Errorf("leader: write mcp config: %w", err)
	}

	bin := cfg.ClaudePath
	if bin == "" {
		bin = "claude"
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--mcp-config", mcpPath,
		"--append-system-prompt", cfg.SystemPrompt,
	}
	env := append([]string{}, os.Environ()...)
	env = append(env, cfg.ExtraEnv...)

	proc, err := cfg.Transport.Start(ctx, transport.Command{
		Path: bin,
		Args: args,
		Env:  env,
		Dir:  cfg.WorkingDir,
	})
	if err != nil {
		return nil, fmt.Errorf("leader: start: %w", err)
	}

	l := &ClaudeLeader{
		cfg:    cfg,
		proc:   proc,
		stdin:  proc.Stdin(),
		events: make(chan AssistantEvent, 32),
	}
	go l.readStdout(proc.Stdout())
	go l.readStderr(proc.Stderr())
	return l, nil
}

// Send delivers a user message to the Leader.
func (l *ClaudeLeader) Send(_ context.Context, text string) error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	if l.closed {
		return ErrClosed
	}
	body, err := encodeUserMessage(text)
	if err != nil {
		return err
	}
	return writeJSONLine(l.stdin, body)
}

// Events returns the channel the chat UI reads from.
func (l *ClaudeLeader) Events() <-chan AssistantEvent { return l.events }

// Close terminates the subprocess and closes the events channel.
func (l *ClaudeLeader) Close() error {
	l.closeMu.Lock()
	if l.closed {
		l.closeMu.Unlock()
		return nil
	}
	l.closed = true
	l.closeMu.Unlock()

	_ = l.stdin.Close()
	_ = l.proc.Kill()
	_ = l.proc.Wait()
	return nil
}

func (l *ClaudeLeader) readStdout(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var ev streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						l.emit(AssistantEvent{Kind: EventAssistantText, Text: c.Text})
					}
				case "tool_use":
					l.emit(AssistantEvent{Kind: EventToolUse, ToolName: c.Name})
				}
			}
		case "result":
			if ev.Result != "" {
				l.emit(AssistantEvent{Kind: EventResult, Text: ev.Result})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		l.emit(AssistantEvent{Kind: EventError, Text: err.Error()})
	}
	close(l.events)
}

func (l *ClaudeLeader) readStderr(r io.Reader) {
	// Pipe stderr to the operator's terminal verbatim so login URLs, MCP
	// errors etc. are visible without us re-encoding them.
	_, _ = io.Copy(os.Stderr, r)
}

func (l *ClaudeLeader) emit(ev AssistantEvent) {
	select {
	case l.events <- ev:
	default:
		// drop if the consumer is too slow to keep up
	}
}

// writeLocalMCPConfig writes an MCP client config pointing at the
// orchestrator and returns its path. Today the config always lives on
// the local filesystem; for an SSH-placed Leader the path is the local
// box's path — Claude Code on the remote host can't read it. SSH Leaders
// will gain a `scp`-style upload step in a follow-up.
func writeLocalMCPConfig(endpoint string) (string, error) {
	body, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"teem": map[string]any{
				"type": "http",
				"url":  endpoint,
			},
		},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "teem-leader-mcp-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	return f.Name(), nil
}
