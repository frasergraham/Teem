package llm

import (
	"context"
	"errors"
)

// Role is the speaker role for a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn in a conversation.
type Message struct {
	Role    Role
	Content string
}

// CompletionRequest is a request to the model.
type CompletionRequest struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
}

// CompletionResponse is the model's reply.
type CompletionResponse struct {
	Model   string
	Content string
	Stop    string
}

// StreamChunk is one delta in a streaming response.
type StreamChunk struct {
	Delta string
	Done  bool
	Err   error
}

// Client is a one-shot LLM client used by utility code paths in Teem.
type Client interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
	Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error)
}

// ErrNoAPIKey is returned when an Anthropic key is required but not set.
var ErrNoAPIKey = errors.New("llm: ANTHROPIC_API_KEY not set")
