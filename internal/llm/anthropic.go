package llm

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicClient implements Client against the Anthropic Messages API.
type AnthropicClient struct {
	client anthropic.Client
	model  anthropic.Model
}

// NewAnthropic returns a client reading ANTHROPIC_API_KEY from env.
// If model is empty, a sensible default is chosen.
func NewAnthropic(model string) (*AnthropicClient, error) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return nil, ErrNoAPIKey
	}
	c := anthropic.NewClient(option.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
	m := anthropic.Model(model)
	if model == "" {
		m = anthropic.ModelClaudeSonnet4_6
	}
	return &AnthropicClient{client: c, model: m}, nil
}

func (a *AnthropicClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	params := a.toParams(req)
	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("anthropic: complete: %w", err)
	}
	var text string
	for _, block := range msg.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return CompletionResponse{
		Model:   string(msg.Model),
		Content: text,
		Stop:    string(msg.StopReason),
	}, nil
}

func (a *AnthropicClient) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error) {
	params := a.toParams(req)
	stream := a.client.Messages.NewStreaming(ctx, params)
	out := make(chan StreamChunk, 16)
	go func() {
		defer close(out)
		for stream.Next() {
			ev := stream.Current()
			// Text delta events carry partial text under Delta.Text.
			if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
				select {
				case out <- StreamChunk{Delta: ev.Delta.Text}:
				case <-ctx.Done():
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			out <- StreamChunk{Err: err}
			return
		}
		out <- StreamChunk{Done: true}
	}()
	return out, nil
}

func (a *AnthropicClient) toParams(req CompletionRequest) anthropic.MessageNewParams {
	model := a.model
	if req.Model != "" {
		model = anthropic.Model(req.Model)
	}
	maxTok := int64(req.MaxTokens)
	if maxTok == 0 {
		maxTok = 1024
	}
	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: maxTok,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for _, m := range req.Messages {
		block := anthropic.NewTextBlock(m.Content)
		switch m.Role {
		case RoleAssistant:
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(block))
		default:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(block))
		}
	}
	return params
}
