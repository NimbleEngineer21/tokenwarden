package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"
)

func TestSlogRecord(t *testing.T) {
	var buf bytes.Buffer
	r := NewSlog(slog.New(slog.NewJSONHandler(&buf, nil)))
	ev := &Event{
		Time:              time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		RequestID:         "req_tw_1",
		UpstreamRequestID: "req_mock_1",
		KeyID:             "0de84bb906b9",
		Team:              "red",
		User:              "alice",
		Model:             "claude-test",
		Stream:            true,
		Status:            200,
		Outcome:           OutcomeOK,
		Usage:             Usage{InputTokens: 3, OutputTokens: 7, CacheCreationInputTokens: 1, CacheReadInputTokens: 2},
		Policy:            Policy{Rule: "everyone", Reason: "ok for all", WouldDeny: true, Revision: "abc123"},
		Duration:          1500 * time.Millisecond,
	}
	if err := r.Record(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	wantKeys := []string{
		"duration", "error_type", "event_time", "key_id", "level", "model", "msg", "outcome", "policy",
		"request_id", "status", "stream", "team", "time", "upstream_request_id", "usage", "user",
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
	for k, want := range map[string]any{
		"msg": "audit", "level": "INFO", "team": "red", "outcome": "ok", "status": 200.0,
		"stream": true, "event_time": "2026-11-05T12:00:00Z", "duration": 1.5e9,
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	// A decoder that keeps one value per key must still see both times.
	logged, _ := got["time"].(string)
	if ts, err := time.Parse(time.RFC3339Nano, logged); err != nil || ts.Equal(ev.Time) {
		t.Errorf("time = %q, want the record time, apart from event_time", logged)
	}
	if dup := duplicateKey(t, buf.Bytes()); dup != "" {
		t.Errorf("log line has key %q twice: %s", dup, buf.String())
	}
	u, _ := got["usage"].(map[string]any)
	for k, want := range map[string]float64{
		"input_tokens": 3, "output_tokens": 7, "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2,
	} {
		if u[k] != want {
			t.Errorf("usage.%s = %v, want %v", k, u[k], want)
		}
	}
	p, _ := got["policy"].(map[string]any)
	for k, want := range map[string]any{
		"rule": "everyone", "reason": "ok for all", "would_deny": true, "revision": "abc123",
	} {
		if p[k] != want {
			t.Errorf("policy.%s = %v, want %v", k, p[k], want)
		}
	}
	if len(p) != 4 {
		t.Errorf("policy = %v, want 4 keys", p)
	}
}

// duplicateKey returns the first top-level key that appears twice in the JSON
// object line, or "" when every key is unique. A map decode cannot see this.
func duplicateKey(t *testing.T, line []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("log line %q: want a JSON object", line)
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		key, _ := tok.(string)
		if seen[key] {
			return key
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return ""
}
