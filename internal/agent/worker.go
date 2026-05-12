package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
)

// jobMessage is the payload format used on the bus job topic.
type jobMessage struct {
	JobID   string `json:"job_id"`
	Prompt  string `json:"prompt"`
	Context string `json:"context,omitempty"`
}

// resultMessage is the payload format used on the bus result topic.
type resultMessage struct {
	JobID  string `json:"job_id"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// Worker drives a single provisioned agent: it pulls jobs from the bus,
// executes each one by shelling out to `claude -p`, and publishes the
// final assistant text back to the bus.
type Worker struct {
	Agent *provisioner.Agent
	Bus   bus.Bus

	jobsTopic   string
	resultTopic string
	logTopic    string
}

// JobsTopic returns the bus topic this worker listens on for jobs.
func JobsTopic(agentID string) string { return "agent." + agentID + ".jobs" }

// ResultsTopic returns the bus topic this worker publishes results to.
func ResultsTopic(agentID string) string { return "agent." + agentID + ".results" }

// LogsTopic returns the bus topic this worker streams logs/status on.
func LogsTopic(agentID string) string { return "agent." + agentID + ".log" }

// Start runs the worker loop in a goroutine. It returns immediately. The
// goroutine exits when ctx is cancelled or the jobs channel closes.
func (w *Worker) Start(ctx context.Context) error {
	w.jobsTopic = JobsTopic(w.Agent.ID)
	w.resultTopic = ResultsTopic(w.Agent.ID)
	w.logTopic = LogsTopic(w.Agent.ID)
	ch, err := w.Bus.Subscribe(ctx, w.jobsTopic)
	if err != nil {
		return fmt.Errorf("agent %s: subscribe: %w", w.Agent.ID, err)
	}
	go func() {
		for msg := range ch {
			var job jobMessage
			if err := json.Unmarshal(msg.Payload, &job); err != nil {
				w.publishLog(ctx, fmt.Sprintf("decode job: %v", err))
				continue
			}
			w.runJob(ctx, job)
		}
	}()
	return nil
}

func (w *Worker) runJob(ctx context.Context, job jobMessage) {
	output, err := w.execClaude(ctx, job)
	res := resultMessage{JobID: job.JobID, Output: output}
	if err != nil {
		res.Error = err.Error()
	}
	body, _ := json.Marshal(res)
	_ = w.Bus.Publish(ctx, bus.Message{
		Topic:   w.resultTopic,
		Kind:    bus.KindResult,
		From:    w.Agent.ID,
		Payload: body,
	})
}

func (w *Worker) execClaude(ctx context.Context, job jobMessage) (string, error) {
	mcpPath, cleanup, err := writeMCPConfig(w.Agent.MCPs)
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
		Dir:  w.Agent.WorkingDir,
		Env:  os.Environ(),
	}
	proc, err := w.Agent.Transport.Start(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	if _, err := io.WriteString(proc.Stdin(), prompt); err != nil {
		_ = proc.Kill()
		return "", fmt.Errorf("stdin: %w", err)
	}
	_ = proc.Stdin().Close()

	final, parseErr := parseClaudeStreamJSON(proc.Stdout())
	if err := proc.Wait(); err != nil {
		return final, fmt.Errorf("claude exit: %w", err)
	}
	if parseErr != nil {
		return final, parseErr
	}
	return final, nil
}

// parseClaudeStreamJSON consumes Claude Code's stream-json output and
// returns the final assistant text. We tolerate unrecognised event types
// because the schema evolves; only the "result" event is required.
func parseClaudeStreamJSON(r io.Reader) (string, error) {
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

func (w *Worker) publishLog(ctx context.Context, line string) {
	_ = w.Bus.Publish(ctx, bus.Message{
		Topic:   w.logTopic,
		Kind:    bus.KindLog,
		From:    w.Agent.ID,
		Payload: []byte(line),
	})
}

// writeMCPConfig writes a Claude Code MCP config JSON to a temp file and
// returns its path along with a cleanup func. If no MCPs are declared it
// returns ("", noop, nil).
func writeMCPConfig(refs []team.MCPRef) (string, func(), error) {
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

// EnsureDir creates dir if it does not exist; used by spawner to set up
// worker WorkingDirs ahead of time.
func EnsureDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Clean(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return nil
}
