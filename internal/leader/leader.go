package leader

import (
	"context"
	"errors"
	"io"

	"github.com/frasergraham/teem/internal/transport"
)

// EventKind enumerates the subset of stream-json events the chat UI needs.
type EventKind string

const (
	EventAssistantText EventKind = "assistant_text"
	EventToolUse       EventKind = "tool_use"
	EventResult        EventKind = "result"
	EventError         EventKind = "error"
)

// AssistantEvent is what the UI consumes.
type AssistantEvent struct {
	Kind EventKind
	// Text is the assistant text (for EventAssistantText/EventResult) or
	// the error message (for EventError).
	Text string
	// ToolName is populated for EventToolUse.
	ToolName string
}

// Config configures a Leader instance.
type Config struct {
	// Transport places the `claude` subprocess. LocalTransport for local,
	// SSHTransport for remote. The chat UI doesn't change either way.
	Transport transport.Transport
	// MCPEndpoint is the URL the Leader should connect to as an MCP
	// client (the orchestrator's streamable-HTTP endpoint).
	MCPEndpoint string
	// SystemPrompt is the assembled Leader system prompt (preamble +
	// roster + project brief).
	SystemPrompt string
	// WorkingDir is the cwd the `claude` subprocess runs in.
	WorkingDir string
	// ExtraEnv is appended to the subprocess environment. PATH and
	// HOME/USER are expected to come from the caller's os.Environ().
	ExtraEnv []string
	// ClaudePath overrides the binary name. Defaults to "claude".
	ClaudePath string
}

// Leader is the chat-UI-facing interface. Concrete implementations vary
// by where the claude subprocess lives.
type Leader interface {
	Send(ctx context.Context, text string) error
	Events() <-chan AssistantEvent
	Close() error
}

// ErrClosed is returned by Send after Close.
var ErrClosed = errors.New("leader: closed")

// writeJSONLine writes one JSON line followed by '\n' to w.
func writeJSONLine(w io.Writer, body []byte) error {
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}
