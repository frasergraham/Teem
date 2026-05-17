package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
	"github.com/frasergraham/teem/internal/usage"
)

// ProcessExecutor runs `claude -p ...` as a subprocess via a transport. It
// is the executor used for local and SSH-placed workers — the leader owns
// the subprocess directly.
type ProcessExecutor struct {
	Transport  transport.Transport
	WorkingDir string
	MCPs       []team.MCPRef

	// AgentID, when set, is used to organize transcript files by agent
	// (one subdirectory per agent under TranscriptDir).
	AgentID string
	// TranscriptDir, when set, is the directory under which each job's
	// raw stream-json transcript is written as <agent_id>/<job_id>.jsonl.
	// Empty disables transcript capture.
	TranscriptDir string

	// OnUsage, when set, is called once per Execute with the
	// per-subprocess token-usage rollup parsed out of the stream-json.
	// Wired by the in-process worker (agent.Worker) and the
	// teem-worker daemon to emit an audit.KindUsageEvent.
	// docs/usage-capture.md is the shared design. Optional — nil
	// disables the callback (e.g. unit tests).
	OnUsage func(jobID string, summary usage.UsageSummary)
}

// NewProcess constructs a ProcessExecutor.
func NewProcess(t transport.Transport, workingDir string, mcps []team.MCPRef) *ProcessExecutor {
	return &ProcessExecutor{Transport: t, WorkingDir: workingDir, MCPs: mcps}
}

// TranscriptPath returns the path the executor would (or did) write the
// transcript for a job to. Returns "" when transcript capture is
// disabled (no TranscriptDir or AgentID).
func (p *ProcessExecutor) TranscriptPath(jobID string) string {
	if p.TranscriptDir == "" || p.AgentID == "" || jobID == "" {
		return ""
	}
	return filepath.Join(p.TranscriptDir, p.AgentID, jobID+".jsonl")
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
		// --verbose is required by Claude Code when stream-json
		// output is requested with -p; without it claude errors
		// out before producing any events.
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if mcpPath != "" {
		args = append(args, "--mcp-config", mcpPath)
	}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	// Skill loading: claude has no --load-skill flag, so the next
	// best thing is a system-prompt instruction that tells the
	// assistant to invoke the named skill via the Skill tool. Skills
	// auto-discover from ~/.claude/skills/ and the plugin install
	// path; this just nudges the worker to use the right one.
	if job.Skill != "" {
		args = append(args, "--append-system-prompt",
			fmt.Sprintf("You have the %q skill available. Invoke it via the Skill tool when it applies to the task at hand.", job.Skill))
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

	var sink io.Writer
	if path := p.TranscriptPath(job.ID); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("transcript mkdir: %w", err)
		}
		tf, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return "", fmt.Errorf("transcript open: %w", err)
		}
		defer tf.Close()
		sink = tf
	}

	cap := usage.NewCapture(time.Now())
	res, parseErr := ParseClaudeStreamJSON(proc.Stdout(), sink, cap)
	waitErr := proc.Wait()
	// Emit usage even on error — partial counts still inform the
	// throttle and the cost-attribution view marks them visibly.
	if p.OnUsage != nil {
		p.OnUsage(job.ID, cap.Summary())
	}
	if waitErr != nil {
		return res.FinalText, fmt.Errorf("claude exit: %w", waitErr)
	}
	if parseErr != nil {
		return res.FinalText, parseErr
	}
	return res.FinalText, nil
}

// StreamResult is the parsed return value from ParseClaudeStreamJSON.
type StreamResult struct {
	FinalText  string
	EventCount int
}

// ParseClaudeStreamJSON consumes Claude Code's stream-json output and
// returns the final assistant text plus an event count. When sink is
// non-nil each input line (with its trailing newline) is teed through
// verbatim — callers can capture the full transcript without buffering
// it all in memory. Tolerates unrecognised event types because the
// schema evolves; only "result" and "assistant" are read.
//
// Each line is also fed through cap (when non-nil) so the shared
// internal/usage parser stays the single source of truth for
// stream-json schema decisions — see docs/usage-capture.md. cap is
// optional; tests that don't care pass nil.
func ParseClaudeStreamJSON(r io.Reader, sink io.Writer, cap *usage.Capture) (StreamResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var res StreamResult
	for scanner.Scan() {
		line := scanner.Bytes()
		if sink != nil {
			if _, err := sink.Write(line); err != nil {
				return res, fmt.Errorf("transcript write: %w", err)
			}
			if _, err := sink.Write([]byte{'\n'}); err != nil {
				return res, fmt.Errorf("transcript write: %w", err)
			}
		}
		if cap != nil {
			cap.Feed(line)
		}
		res.EventCount++
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
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "result":
			if ev.Result != "" {
				res.FinalText = ev.Result
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					res.FinalText = c.Text
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("read stream: %w", err)
	}
	return res, nil
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
