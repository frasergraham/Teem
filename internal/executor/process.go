package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
)

// ProcessExecutor runs `claude -p ...` as a subprocess via a transport. It
// is the executor used for local and SSH-placed workers — the leader owns
// the subprocess directly.
type ProcessExecutor struct {
	Transport  transport.Transport
	WorkingDir string
	MCPs       []team.MCPRef
}

// NewProcess constructs a ProcessExecutor.
func NewProcess(t transport.Transport, workingDir string, mcps []team.MCPRef) *ProcessExecutor {
	return &ProcessExecutor{Transport: t, WorkingDir: workingDir, MCPs: mcps}
}

// Execute runs claude for the supplied job and returns the final assistant
// text. Per-job MCPs (set on Job) take precedence over the executor's
// configured MCPs; this matches the design for remote workers where the
// leader ships MCP refs in the job payload.
func (p *ProcessExecutor) Execute(ctx context.Context, job Job) (string, error) {
	mcps := p.MCPs
	if len(job.MCPs) > 0 {
		mcps = job.MCPs
	}
	mcpPath, cleanup, err := WriteMCPConfig(mcps)
	if err != nil {
		return "", fmt.Errorf("mcp config: %w", err)
	}
	defer cleanup()

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
	}
	if mcpPath != "" {
		args = append(args, "--mcp-config", mcpPath)
	}
	prompt := job.Prompt
	if job.Context != "" {
		prompt = job.Context + "\n\n" + prompt
	}

	cmd := transport.Command{
		Path: "claude",
		Args: args,
		Dir:  p.WorkingDir,
		Env:  os.Environ(),
	}
	proc, err := p.Transport.Start(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	if _, err := io.WriteString(proc.Stdin(), prompt); err != nil {
		_ = proc.Kill()
		return "", fmt.Errorf("stdin: %w", err)
	}
	_ = proc.Stdin().Close()

	final, parseErr := ParseClaudeStreamJSON(proc.Stdout())
	if err := proc.Wait(); err != nil {
		return final, fmt.Errorf("claude exit: %w", err)
	}
	if parseErr != nil {
		return final, parseErr
	}
	return final, nil
}

// ParseClaudeStreamJSON consumes Claude Code's stream-json output and
// returns the final assistant text. Tolerates unrecognised event types
// because the schema evolves; only "result" and "assistant" are read.
func ParseClaudeStreamJSON(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var final string
	for scanner.Scan() {
		var ev struct {
			Type    string          `json:"type"`
			Result  string          `json:"result"`
			Content json.RawMessage `json:"content"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "result":
			if ev.Result != "" {
				final = ev.Result
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					final = c.Text
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return final, fmt.Errorf("read stream: %w", err)
	}
	return final, nil
}

// WriteMCPConfig writes a Claude Code MCP config JSON to a temp file and
// returns its path along with a cleanup func. Returns ("", noop, nil) when
// refs is empty.
func WriteMCPConfig(refs []team.MCPRef) (string, func(), error) {
	if len(refs) == 0 {
		return "", func() {}, nil
	}
	type httpEntry struct {
		Type string            `json:"type"`
		URL  string            `json:"url"`
		Env  map[string]string `json:"env,omitempty"`
	}
	type stdioEntry struct {
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
	}
	servers := map[string]any{}
	for _, r := range refs {
		if r.URL != "" {
			servers[r.Name] = httpEntry{Type: "http", URL: r.URL, Env: r.Env}
		} else {
			servers[r.Name] = stdioEntry{Command: r.Command, Args: r.Args, Env: r.Env}
		}
	}
	body, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp("", "teem-mcp-*.json")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	_ = f.Close()
	path := f.Name()
	return path, func() { _ = os.Remove(path) }, nil
}
