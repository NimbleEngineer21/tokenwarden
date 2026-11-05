package mock

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(Config{}))
	t.Cleanup(ts.Close)
	return ts
}

// result is the part of a response that the tests read. do closes the body,
// so it returns no *http.Response.
type result struct {
	StatusCode int
	Header     http.Header
}

func do(t *testing.T, method, url, body string) (result, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header}, string(b), err
}

func setFault(t *testing.T, ts *httptest.Server, fault string) {
	t.Helper()
	resp, body, err := do(t, http.MethodPost, ts.URL+"/_faults", fault)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("set fault %s: %d %s %v", fault, resp.StatusCode, body, err)
	}
}

func TestStatusFaults(t *testing.T) {
	tests := []struct {
		status  int
		errType string
	}{
		{429, anthropic.ErrRateLimit},
		{500, anthropic.ErrAPI},
		{529, anthropic.ErrOverloaded},
	}
	ts := newTestServer(t)
	for _, tt := range tests {
		setFault(t, ts, `{"status":`+strconv.Itoa(tt.status)+`,"count":1}`)
		resp, body, err := do(t, http.MethodPost, ts.URL+"/v1/messages", msgBody("x", 10, true, ""))
		if err != nil || resp.StatusCode != tt.status {
			t.Fatalf("status %d (%v), want %d", resp.StatusCode, err, tt.status)
		}
		var eb anthropic.ErrorBody
		if err := json.Unmarshal([]byte(body), &eb); err != nil || eb.Error.Type != tt.errType {
			t.Fatalf("body %s, want type %s", body, tt.errType)
		}
		if eb.RequestID == "" || eb.RequestID != resp.Header.Get("request-id") {
			t.Fatalf("request_id %q, header %q", eb.RequestID, resp.Header.Get("request-id"))
		}
		if got := resp.Header.Get("retry-after"); (tt.status == 429) != (got != "") {
			t.Fatalf("status %d: retry-after %q", tt.status, got)
		}
		if resp, _, _ := do(t, http.MethodPost, ts.URL+"/v1/messages", msgBody("x", 10, true, "")); resp.StatusCode != http.StatusOK {
			t.Fatalf("count 1: second request got %d", resp.StatusCode)
		}
	}
}

func TestDropFaults(t *testing.T) {
	ts := newTestServer(t)
	for _, mid := range []bool{false, true} {
		fault := `{"drop_after_events":3}`
		if mid {
			fault = `{"drop_after_events":3,"drop_mid_event":true}`
		}
		setFault(t, ts, fault)
		resp, body, err := do(t, http.MethodPost, ts.URL+"/v1/messages", msgBody("drop", 100, true, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("mid=%t: body read error %v, want io.ErrUnexpectedEOF", mid, err)
		}
		events := strings.Count(body, "\n\n")
		tail := body[strings.LastIndex(body, "\n\n")+2:]
		if events != 3 {
			t.Fatalf("mid=%t: %d complete events, want 3", mid, events)
		}
		if mid == (tail == "") {
			t.Fatalf("mid=%t: partial tail %q", mid, tail)
		}
		if strings.Contains(body, anthropic.EventMessageStop) {
			t.Fatalf("mid=%t: message_stop arrived", mid)
		}
		rd := anthropic.NewReader(strings.NewReader(body), anthropic.Limits{})
		var rerr error
		for rerr == nil {
			_, rerr = rd.Next()
		}
		// A drop at an event boundary looks like a clean end to the Reader;
		// only a drop inside an event is a Reader error.
		if mid != errors.Is(rerr, io.ErrUnexpectedEOF) {
			t.Fatalf("mid=%t: Reader ended with %v", mid, rerr)
		}
	}
}

func TestLongReplyFault(t *testing.T) {
	ts := newTestServer(t)
	setFault(t, ts, `{"long_reply":true}`)
	_, body, err := do(t, http.MethodPost, ts.URL+"/v1/messages", msgBody("long", 700, true, ""))
	if err != nil {
		t.Fatal(err)
	}
	res := parseStream(t, body)
	if res.usage.OutputTokens != 700 || res.stop != stopMaxTokens {
		t.Fatalf("output_tokens %d, stop %q; want 700, max_tokens", res.usage.OutputTokens, res.stop)
	}
}

func TestFaultLifecycle(t *testing.T) {
	ts := newTestServer(t)
	get := func() string {
		_, body, err := do(t, http.MethodGet, ts.URL+"/_faults", "")
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := get(); got != "{}" {
		t.Fatalf("initial fault = %s", got)
	}
	setFault(t, ts, `{"status":529,"delay":"1ms","count":2}`)
	if resp, _, _ := do(t, http.MethodPost, ts.URL+"/v1/messages", msgBody("x", 10, false, "")); resp.StatusCode != 529 {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	if got := get(); got != `{"delay":"1ms","status":529,"count":1}` {
		t.Fatalf("after one use: %s", got)
	}
	// An invalid request is rejected before a fault applies.
	if resp, _, _ := do(t, http.MethodPost, ts.URL+"/v1/messages", "{}"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid request: %d", resp.StatusCode)
	}
	if resp, _, _ := do(t, http.MethodPost, ts.URL+"/v1/messages",
		msgBody("x", 10, false, `,"tool_choice":{"type":"any"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad tool_choice: %d", resp.StatusCode)
	}
	if got := get(); !strings.Contains(got, `"count":1`) {
		t.Fatalf("invalid request used the fault: %s", got)
	}
	resp, _, _ := do(t, http.MethodDelete, ts.URL+"/_faults", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	if got := get(); got != "{}" {
		t.Fatalf("after DELETE: %s", got)
	}
	if resp, _, _ := do(t, http.MethodPost, ts.URL+"/v1/messages", msgBody("x", 10, false, "")); resp.StatusCode != http.StatusOK {
		t.Fatalf("after DELETE: %d", resp.StatusCode)
	}
}

func TestFaultValidation(t *testing.T) {
	ts := newTestServer(t)
	for _, body := range []string{
		`{"status":418}`,
		`{"delay":"soon"}`,
		`{"delay":"-1s"}`,
		`{"delay":"11m"}`,
		`{"count":-1}`,
		`{"drop_after_events":-1}`,
		`{"drop_mid_event":true}`,
		`{"stauts":429}`,
		`not json`,
	} {
		resp, _, _ := do(t, http.MethodPost, ts.URL+"/_faults", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, resp.StatusCode)
		}
	}
	resp, _, _ := do(t, http.MethodPut, ts.URL+"/_faults", "{}")
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") == "" {
		t.Fatalf("PUT: %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}
