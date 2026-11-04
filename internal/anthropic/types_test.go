package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// stream follows the event sequence in the Messages API streaming docs, with
// a text block, a tool_use block, an unknown event type, and cumulative usage.
const stream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":25,"cache_read_input_tokens":7,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_test","name":"mcp__github__create_issue","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"title\":"}}

event: future_event
data: {"type":"future_event","new_field":true}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":25,"output_tokens":40}}

event: message_stop
data: {"type":"message_stop"}

`

func TestParseStream(t *testing.T) {
	evs, err := readAll(t, stream, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var (
		usage    Usage
		text     strings.Builder
		tools    []string
		stop     string
		types    []string
		rawBytes []byte
	)
	for _, e := range evs {
		rawBytes = append(rawBytes, e.Raw...)
		ev, err := ParseStreamEvent(e.Data)
		if err != nil {
			t.Fatalf("ParseStreamEvent(%s): %v", e.Data, err)
		}
		if ev.Type != e.Name {
			t.Errorf("data type %q != event name %q", ev.Type, e.Name)
		}
		types = append(types, ev.Type)
		switch ev.Type {
		case EventMessageStart:
			usage.Merge(ev.Message.Usage)
		case EventContentBlockStart:
			if ev.ContentBlock.Type == BlockToolUse {
				tools = append(tools, ev.ContentBlock.Name)
			}
		case EventContentBlockDelta:
			if ev.Delta.Type == DeltaText {
				text.WriteString(ev.Delta.Text)
			}
		case EventMessageDelta:
			usage.Merge(*ev.Usage)
			stop = ev.Delta.StopReason
		}
	}
	if !bytes.Equal(rawBytes, []byte(stream)) {
		t.Error("concatenated raw bytes differ from the stream")
	}
	if got := len(types); got != 11 {
		t.Errorf("got %d events, want 11", got)
	}
	want := Usage{InputTokens: 25, OutputTokens: 40, CacheReadInputTokens: 7}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
	if text.String() != "Hi" {
		t.Errorf("text = %q", text.String())
	}
	if len(tools) != 1 || tools[0] != "mcp__github__create_issue" {
		t.Errorf("tools = %v", tools)
	}
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q", stop)
	}
}

func TestUsageMergeIsNotAdditive(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 1}
	u.Merge(Usage{OutputTokens: 5})
	u.Merge(Usage{OutputTokens: 9})
	if want := (Usage{InputTokens: 10, OutputTokens: 9}); u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}
}

func TestWriteError(t *testing.T) {
	var b bytes.Buffer
	if err := NewWriter(&b).WriteError(ErrRateLimit, "team budget exhausted", ""); err != nil {
		t.Fatal(err)
	}
	evs, err := readAll(t, b.String(), Limits{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("read back: %v, %d events", err, len(evs))
	}
	if evs[0].Name != EventError {
		t.Errorf("name = %q", evs[0].Name)
	}
	var body ErrorBody
	if err := json.Unmarshal(evs[0].Data, &body); err != nil {
		t.Fatal(err)
	}
	want := ErrorBody{Type: "error", Error: APIError{Type: "rate_limit_error", Message: "team budget exhausted"}}
	if body != want {
		t.Errorf("body = %+v, want %+v", body, want)
	}
	if bytes.Contains(evs[0].Data, []byte("request_id")) {
		t.Error("empty request_id was not omitted")
	}
}
