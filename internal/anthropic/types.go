package anthropic

import (
	"encoding/json"
)

// Stream event types of the Messages API. The API may add types; callers
// must forward unknown types unchanged.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// Delta types inside content_block_delta.
const (
	DeltaText      = "text_delta"
	DeltaInputJSON = "input_json_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
)

// Content block types that carry a tool name.
const (
	BlockToolUse       = "tool_use"
	BlockServerToolUse = "server_tool_use"
)

// Error types of the Messages API error body.
const (
	ErrInvalidRequest  = "invalid_request_error"
	ErrAuthentication  = "authentication_error"
	ErrBilling         = "billing_error"
	ErrPermission      = "permission_error"
	ErrNotFound        = "not_found_error"
	ErrConflict        = "conflict_error"
	ErrRequestTooLarge = "request_too_large"
	ErrRateLimit       = "rate_limit_error"
	ErrAPI             = "api_error"
	ErrTimeout         = "timeout_error"
	ErrOverloaded      = "overloaded_error"
)

// StreamEvent is the subset of a stream event payload that the gateway reads.
// One flat struct covers every event type; fields that an event type does not
// carry stay zero.
type StreamEvent struct {
	Type string `json:"type"`
	// Message is set on message_start.
	Message *MessageStart `json:"message,omitempty"`
	// Index is the content block index on content_block_* events.
	Index int `json:"index"`
	// ContentBlock is set on content_block_start.
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	// Delta is set on content_block_delta and message_delta.
	Delta *Delta `json:"delta,omitempty"`
	// Usage is set on message_delta. Its counts are cumulative.
	Usage *Usage `json:"usage,omitempty"`
	// Error is set on error.
	Error *APIError `json:"error,omitempty"`
}

// MessageStart is the message object in message_start.
type MessageStart struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
}

// ContentBlock is the block header in content_block_start.
type ContentBlock struct {
	Type string `json:"type"`
	// ID and Name are set on tool_use and server_tool_use blocks.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Delta is the delta object of content_block_delta or message_delta.
type Delta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

// Usage holds token counts.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Merge folds a later usage report into u. Stream usage counts are
// cumulative and a report may omit a field, so the larger value per field is
// the current total; adding the reports would count tokens twice.
func (u *Usage) Merge(later Usage) {
	u.InputTokens = max(u.InputTokens, later.InputTokens)
	u.OutputTokens = max(u.OutputTokens, later.OutputTokens)
	u.CacheCreationInputTokens = max(u.CacheCreationInputTokens, later.CacheCreationInputTokens)
	u.CacheReadInputTokens = max(u.CacheReadInputTokens, later.CacheReadInputTokens)
}

// APIError is the error object of the Messages API error body.
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ErrorBody is the Messages API error body. The same JSON is the HTTP body of
// a failed request and the data of an error stream event.
type ErrorBody struct {
	Type      string   `json:"type"`
	Error     APIError `json:"error"`
	RequestID string   `json:"request_id,omitempty"`
}

// ParseStreamEvent decodes the data of one stream event. Unknown fields and
// unknown event types are not errors.
func ParseStreamEvent(data []byte) (StreamEvent, error) {
	var ev StreamEvent
	err := json.Unmarshal(data, &ev)
	return ev, err
}

// NewErrorBody returns the JSON error body for errType and message.
func NewErrorBody(errType, message, requestID string) []byte {
	b, err := json.Marshal(ErrorBody{
		Type:      EventError,
		Error:     APIError{Type: errType, Message: message},
		RequestID: requestID,
	})
	if err != nil {
		// Marshaling a struct of strings cannot fail.
		panic(err)
	}
	return b
}

// WriteError sends an error stream event, the way the API reports a failure
// after the stream has started.
func (w *Writer) WriteError(errType, message, requestID string) error {
	return w.WriteEvent(EventError, NewErrorBody(errType, message, requestID))
}
