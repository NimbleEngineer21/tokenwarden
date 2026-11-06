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
		"duration", "error_type", "key_id", "level", "model", "msg", "outcome", "request_id",
		"status", "stream", "team", "time", "upstream_request_id", "usage", "user",
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
		"stream": true, "time": "2026-11-05T12:00:00Z", "duration": 1.5e9,
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	u, _ := got["usage"].(map[string]any)
	for k, want := range map[string]float64{
		"input_tokens": 3, "output_tokens": 7, "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2,
	} {
		if u[k] != want {
			t.Errorf("usage.%s = %v, want %v", k, u[k], want)
		}
	}
}
