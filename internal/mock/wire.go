package mock

import (
	"encoding/json"
	"strings"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// request is the part of a Messages API request that the mock reads.
type request struct {
	Model      string          `json:"model"`
	MaxTokens  int             `json:"max_tokens"`
	Stream     bool            `json:"stream"`
	System     json.RawMessage `json:"system"`
	Messages   []message       `json:"messages"`
	Tools      []tool          `json:"tools"`
	ToolChoice *toolChoice     `json:"tool_choice"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type tool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// inputTokens counts the words of the system prompt and the message text.
func (r *request) inputTokens() int64 {
	n := countWords(r.System)
	for _, m := range r.Messages {
		n += countWords(m.Content)
	}
	return int64(n)
}

// countWords counts the words in a content value, which is a string or an
// array of blocks. Only the "text" field of a block counts.
func countWords(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return len(strings.Fields(s))
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return 0
	}
	n := 0
	for _, b := range blocks {
		n += len(strings.Fields(b.Text))
	}
	return n
}

// The types below encode the full wire payloads. The anthropic package
// decodes only the subset that the gateway reads, so the mock keeps its own
// encoders to send what the real API sends.

type wireMessage struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []wireBlock     `json:"content"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        anthropic.Usage `json:"usage"`
}

type wireBlock struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type wireEvent struct {
	Type         string           `json:"type"`
	Message      *wireMessage     `json:"message,omitempty"`
	Index        *int             `json:"index,omitempty"`
	ContentBlock *wireBlock       `json:"content_block,omitempty"`
	Delta        any              `json:"delta,omitempty"`
	Usage        *anthropic.Usage `json:"usage,omitempty"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type jsonDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type messageDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}
