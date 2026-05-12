package leader

import "encoding/json"

// userInput is the wire format we write to Claude Code stdin.
type userInput struct {
	Type    string      `json:"type"`
	Message userMessage `json:"message"`
}

type userMessage struct {
	Role    string             `json:"role"`
	Content []userContentBlock `json:"content"`
}

type userContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// encodeUserMessage builds the JSON line the Leader subprocess expects on
// stdin for a user prompt.
func encodeUserMessage(text string) ([]byte, error) {
	in := userInput{
		Type: "user",
		Message: userMessage{
			Role:    "user",
			Content: []userContentBlock{{Type: "text", Text: text}},
		},
	}
	return json.Marshal(in)
}

// streamEvent is the union of stream-json events we care about. We only
// decode the fields we use; everything else is ignored to stay forward
// compatible.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	// For "result" events.
	Result string `json:"result,omitempty"`
	// For "assistant" events: nested message.content[]
	Message struct {
		Content []struct {
			Type  string `json:"type"`
			Text  string `json:"text,omitempty"`
			Name  string `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	} `json:"message,omitempty"`
	// For "system" events.
	Session string `json:"session_id,omitempty"`
	// For "stream_event" deltas (claude --include-partial-messages).
	Event json.RawMessage `json:"event,omitempty"`
}
