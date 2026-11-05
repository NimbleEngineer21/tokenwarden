package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

const testKey = "sk-ant-test-mock"

// flushRecorder is an http.ResponseWriter that keeps the bytes of each
// flush and the time it happened. Inside a synctest bubble the time is the
// bubble's fake clock.
type flushRecorder struct {
	mu      sync.Mutex
	start   time.Time
	header  http.Header
	code    int
	pending bytes.Buffer
	flushes []flushed
}

type flushed struct {
	at   time.Duration
	data string
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{start: time.Now(), header: http.Header{}}
}

func (f *flushRecorder) Header() http.Header { return f.header }

func (f *flushRecorder) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.code == 0 {
		f.code = code
	}
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.code == 0 {
		f.code = http.StatusOK
	}
	return f.pending.Write(b)
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes = append(f.flushes, flushed{at: time.Since(f.start), data: f.pending.String()})
	f.pending.Reset()
}

// body returns all bytes written, flushed or not.
func (f *flushRecorder) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for _, fl := range f.flushes {
		sb.WriteString(fl.data)
	}
	sb.WriteString(f.pending.String())
	return sb.String()
}

// msgBody returns a request body. extra holds more top-level JSON fields,
// each with a leading comma.
func msgBody(text string, maxTokens int, stream bool, extra string) string {
	return fmt.Sprintf(`{"model":"claude-test","max_tokens":%d,"stream":%t,"messages":[{"role":"user","content":%q}]%s}`,
		maxTokens, stream, text, extra)
}

func newRequest(ctx context.Context, body string) *http.Request {
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("x-api-key", testKey)
	r.Header.Set("Content-Type", "application/json")
	return r
}

type streamResult struct {
	events []anthropic.StreamEvent
	text   map[int]string // concatenated deltas per block index
	usage  anthropic.Usage
	stop   string
}

func parseStream(t *testing.T, raw string) streamResult {
	t.Helper()
	res := streamResult{text: map[int]string{}}
	rd := anthropic.NewReader(strings.NewReader(raw), anthropic.Limits{})
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return res
		}
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		se, err := anthropic.ParseStreamEvent(ev.Data)
		if err != nil {
			t.Fatalf("parse %s: %v", ev.Name, err)
		}
		if se.Type != ev.Name {
			t.Fatalf("event name %q, data type %q", ev.Name, se.Type)
		}
		res.events = append(res.events, se)
		switch se.Type {
		case anthropic.EventMessageStart:
			res.usage.Merge(se.Message.Usage)
		case anthropic.EventContentBlockDelta:
			res.text[se.Index] += se.Delta.Text + se.Delta.PartialJSON
		case anthropic.EventMessageDelta:
			res.usage.Merge(*se.Usage)
			res.stop = se.Delta.StopReason
		}
	}
}

func serve(t *testing.T, s *Server, body string) *flushRecorder {
	t.Helper()
	rec := newFlushRecorder()
	s.ServeHTTP(rec, newRequest(t.Context(), body))
	return rec
}

func TestRequestErrors(t *testing.T) {
	big := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"` + strings.Repeat("a", maxRequestBytes) + `"}]}`
	tests := []struct {
		name, method, path, body string
		noKey                    bool
		status                   int
		errType                  string
	}{
		{"no key", "POST", "/v1/messages", msgBody("hi", 10, false, ""), true, 401, anthropic.ErrAuthentication},
		{"bad json", "POST", "/v1/messages", "{", false, 400, anthropic.ErrInvalidRequest},
		{"no model", "POST", "/v1/messages", `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, false, 400, anthropic.ErrInvalidRequest},
		{"zero max_tokens", "POST", "/v1/messages", msgBody("hi", 0, false, ""), false, 400, anthropic.ErrInvalidRequest},
		{"no messages", "POST", "/v1/messages", `{"model":"m","max_tokens":1,"messages":[]}`, false, 400, anthropic.ErrInvalidRequest},
		{"unknown tool_choice", "POST", "/v1/messages", msgBody("hi", 10, false, `,"tool_choice":{"type":"maybe"}`), false, 400, anthropic.ErrInvalidRequest},
		{"too large", "POST", "/v1/messages", big, false, 413, anthropic.ErrRequestTooLarge},
		{"unknown path", "POST", "/v1/complete", "{}", false, 404, anthropic.ErrNotFound},
		{"wrong method", "GET", "/v1/messages", "", false, 404, anthropic.ErrNotFound},
	}
	s := New(Config{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader(tt.body))
			if !tt.noKey {
				r.Header.Set("x-api-key", testKey)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tt.status, w.Body)
			}
			var eb anthropic.ErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &eb); err != nil {
				t.Fatalf("error body: %v", err)
			}
			if eb.Type != "error" || eb.Error.Type != tt.errType {
				t.Fatalf("error = %+v, want type %s", eb, tt.errType)
			}
			if id := w.Header().Get("request-id"); id != "" && eb.RequestID != id {
				t.Fatalf("request_id %q, header %q", eb.RequestID, id)
			}
		})
	}
}

func TestDeterministic(t *testing.T) {
	s := New(Config{})
	for _, stream := range []bool{true, false} {
		a := serve(t, s, msgBody("same prompt", 200, stream, "")).body()
		b := serve(t, s, msgBody("same prompt", 200, stream, "")).body()
		c := serve(t, s, msgBody("other prompt", 200, stream, "")).body()
		if a != b {
			t.Errorf("stream=%t: same body gave different replies", stream)
		}
		if a == c {
			t.Errorf("stream=%t: different bodies gave the same reply", stream)
		}
	}
}

func TestStreamSequenceAndUsage(t *testing.T) {
	s := New(Config{})
	for i := range 50 {
		prompt := fmt.Sprintf("prompt number %d", i)
		maxTokens := 10 + i*3
		rec := serve(t, s, msgBody(prompt, maxTokens, true, ""))
		if ct := rec.header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q", ct)
		}
		res := parseStream(t, rec.body())
		var names []string
		for _, ev := range res.events {
			if ev.Type != anthropic.EventContentBlockDelta {
				names = append(names, ev.Type)
			}
		}
		want := []string{
			anthropic.EventMessageStart, anthropic.EventContentBlockStart, anthropic.EventPing,
			anthropic.EventContentBlockStop, anthropic.EventMessageDelta, anthropic.EventMessageStop,
		}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("events = %v, want %v", names, want)
		}
		words := int64(len(strings.Fields(res.text[0])))
		if words != res.usage.OutputTokens {
			t.Fatalf("%d words sent, output_tokens = %d", words, res.usage.OutputTokens)
		}
		if res.usage.OutputTokens > int64(maxTokens) {
			t.Fatalf("output_tokens %d > max_tokens %d", res.usage.OutputTokens, maxTokens)
		}
		if res.usage.InputTokens != 3 {
			t.Fatalf("input_tokens = %d, want 3", res.usage.InputTokens)
		}
		// A reply below the cap ends its turn. A reply at the cap may also
		// have ended its turn on exactly that word.
		if res.usage.OutputTokens < int64(maxTokens) && res.stop != stopEndTurn {
			t.Fatalf("stop_reason = %q below the cap", res.stop)
		}
		if res.stop != stopEndTurn && res.stop != stopMaxTokens {
			t.Fatalf("stop_reason = %q", res.stop)
		}
	}
}

func TestMaxTokensCap(t *testing.T) {
	res := parseStream(t, serve(t, New(Config{}), msgBody("cap me", 5, true, "")).body())
	if res.usage.OutputTokens != 5 || res.stop != stopMaxTokens {
		t.Fatalf("output_tokens = %d, stop = %q; want 5, max_tokens", res.usage.OutputTokens, res.stop)
	}
}

func TestNonStream(t *testing.T) {
	s := New(Config{})
	rec := serve(t, s, msgBody("whole reply", 300, false, ""))
	var m struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage anthropic.Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(rec.body()), &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "message" || m.Role != "assistant" || !strings.HasPrefix(m.ID, "msg_mock_") {
		t.Fatalf("message header = %+v", m)
	}
	if len(m.Content) != 1 || m.Content[0].Type != "text" {
		t.Fatalf("content = %+v", m.Content)
	}
	if n := int64(len(strings.Fields(m.Content[0].Text))); n != m.Usage.OutputTokens {
		t.Fatalf("%d words, output_tokens = %d", n, m.Usage.OutputTokens)
	}
	// The stream form of the same request carries the same text.
	res := parseStream(t, serve(t, s, msgBody("whole reply", 300, true, "")).body())
	if res.text[0] != m.Content[0].Text {
		t.Fatalf("stream text differs from non-stream text")
	}
}

func TestInputTokens(t *testing.T) {
	req := request{
		System: json.RawMessage(`[{"type":"text","text":"be brief please"}]`),
		Messages: []message{
			{Content: json.RawMessage(`"one two"`)},
			{Content: json.RawMessage(`[{"type":"text","text":"three"},{"type":"image"}]`)},
			{Content: json.RawMessage(`42`)},
		},
	}
	if got := req.inputTokens(); got != 6 {
		t.Fatalf("inputTokens = %d, want 6", got)
	}
}
