package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/mock"
)

const testKey = "tw_test_loadgen"

func env(key string) func(string) string {
	return func(name string) string {
		if name == "TW_GATEWAY_KEY" {
			return key
		}
		return ""
	}
}

func TestConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		key  string
		want string
	}{
		{"no key", nil, "", "TW_GATEWAY_KEY is not set"},
		{"bad scheme", []string{"-url", "ftp://x"}, testKey, "want an http or https URL"},
		{"no host", []string{"-url", "http://"}, testKey, "want an http or https URL"},
		{"empty list item", []string{"-url", "http://a,"}, testKey, "want an http or https URL"},
		{"extra arg", []string{"x"}, testKey, `unexpected argument "x"`},
		{"zero concurrency", []string{"-concurrency", "0"}, testKey, "-concurrency must be at least 1"},
		{"negative requests", []string{"-requests", "-1"}, testKey, "must not be negative"},
		{"negative duration", []string{"-duration", "-1s"}, testKey, "must not be negative"},
		{"no limit", []string{"-requests", "0"}, testKey, "set -requests or -duration"},
		{"zero timeout", []string{"-timeout", "0"}, testKey, "-timeout must be positive"},
		{"zero max tokens", []string{"-max-tokens", "0"}, testKey, "-max-tokens must be at least 1"},
		{"negative words", []string{"-prompt-words", "-1"}, testKey, "-prompt-words must not be negative"},
		{"bad flag", []string{"-nope"}, testKey, "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := run(t.Context(), tt.args, env(tt.key), io.Discard, &stderr)
			if !errors.Is(err, errUsage) {
				t.Fatalf("run() = %v, want errUsage", err)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"-url", "http://a:1/, https://b"}, env(testKey), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.urls, " "); got != "http://a:1 https://b" {
		t.Errorf("urls = %q", got)
	}
	if !cfg.stream || cfg.requests != 100 || cfg.concurrency != 10 || cfg.key != testKey {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	if err := run(t.Context(), []string{"-h"}, env(""), io.Discard, io.Discard); err != nil {
		t.Fatalf("run(-h) = %v", err)
	}
	if _, err := parseConfig([]string{"-h"}, env(""), io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseConfig(-h) = %v, want flag.ErrHelp", err)
	}
}

// runJSON runs loadgen with -json and returns the report.
func runJSON(t *testing.T, args ...string) report {
	t.Helper()
	var out bytes.Buffer
	if err := run(t.Context(), append(args, "-json"), env(testKey), &out, io.Discard); err != nil {
		t.Fatalf("run() = %v", err)
	}
	var rep report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode report %q: %v", out.String(), err)
	}
	return rep
}

func TestAgainstMock(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "non-stream"}[stream], func(t *testing.T) {
			srv := httptest.NewServer(mock.New(mock.Config{}))
			t.Cleanup(srv.Close)
			rep := runJSON(t, "-url", srv.URL, "-requests", "6", "-concurrency", "3",
				"-max-tokens", "20", "-prompt-words", "4", "-stream="+map[bool]string{true: "true", false: "false"}[stream])
			if rep.Requests != 6 || rep.Outcomes[outcomeOK] != 6 || len(rep.Outcomes) != 1 {
				t.Fatalf("report = %+v, want 6 ok", rep)
			}
			// Mock input tokens are prompt words: "Request N." plus 4.
			if rep.InputTokens != 6*6 {
				t.Errorf("input tokens = %d, want 36", rep.InputTokens)
			}
			if rep.OutputTokens < 6 || rep.OutputTokens > 6*20 {
				t.Errorf("output tokens = %d, want 6..120", rep.OutputTokens)
			}
			if rep.Latency.N != 6 {
				t.Errorf("latency n = %d, want 6", rep.Latency.N)
			}
			wantTTFT := 0
			if stream {
				wantTTFT = 6
			}
			if rep.TTFT.N != wantTTFT {
				t.Errorf("ttft n = %d, want %d", rep.TTFT.N, wantTTFT)
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	var (
		bad     atomic.Value
		mu      sync.Mutex
		prompts []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			MaxTokens int64  `json:"max_tokens"`
			Stream    bool   `json:"stream"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		err := json.NewDecoder(r.Body).Decode(&body)
		switch {
		case err != nil:
			bad.Store("decode: " + err.Error())
		case r.Method != http.MethodPost || r.URL.Path != "/v1/messages":
			bad.Store("target " + r.Method + " " + r.URL.Path)
		case r.Header.Get("x-api-key") != testKey:
			bad.Store("x-api-key header")
		case r.Header.Get("anthropic-version") == "":
			bad.Store("no anthropic-version")
		case body.Model != "m1" || body.MaxTokens != 7 || body.Stream:
			bad.Store("body fields")
		case len(body.Messages) != 1 || body.Messages[0].Role != "user":
			bad.Store("messages")
		default:
			mu.Lock()
			prompts = append(prompts, body.Messages[0].Content)
			mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	rep := runJSON(t, "-url", srv.URL, "-requests", "2", "-model", "m1", "-max-tokens", "7",
		"-prompt-words", "2", "-stream=false")
	if v := bad.Load(); v != nil {
		t.Fatalf("bad request: %v", v)
	}
	if rep.Outcomes[outcomeOK] != 2 {
		t.Fatalf("outcomes = %v", rep.Outcomes)
	}
	// Each prompt differs, so that a seeded upstream varies its replies.
	slices.Sort(prompts)
	if want := []string{"Request 1. lorem lorem", "Request 2. lorem lorem"}; !slices.Equal(prompts, want) {
		t.Errorf("prompts = %q, want %q", prompts, want)
	}
}

func TestOutcomes(t *testing.T) {
	const (
		start = `event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}}` + "\n\n"
		delta = `event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
		usage = `event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n"
		stop    = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		errEv   = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"x\"}}\n\n"
		errNoTy = "event: error\ndata: {\"type\":\"error\"}\n\n"
	)
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		input   int64
		output  int64
		ttftN   int
		latency int
	}{
		{"ok", 200, start + delta + usage + stop, outcomeOK, 8, 3, 1, 1},
		{"ok without content", 200, start + usage + stop, outcomeOK, 8, 3, 0, 1},
		{"cut by error event", 200, start + delta + errEv, "stream_rate_limit_error", 8, 0, 0, 0},
		{"error event without type", 200, start + errNoTy, "stream_unknown", 8, 0, 0, 0},
		{"no message_stop", 200, start + delta + usage, outcomeTruncated, 8, 3, 0, 0},
		{"bad event data", 200, "data: {\n\n", outcomeBadBody, 0, 0, 0, 0},
		{"budget denial", 429, `{"type":"error"}`, "http_429", 0, 0, 0, 0},
		{"no budget", 403, `{}`, "http_403", 0, 0, 0, 0},
		{"server error", 503, ``, "http_503", 0, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			rep := runJSON(t, "-url", srv.URL, "-requests", "1")
			if rep.Outcomes[tt.want] != 1 || len(rep.Outcomes) != 1 {
				t.Errorf("outcomes = %v, want %s=1", rep.Outcomes, tt.want)
			}
			if rep.InputTokens != tt.input || rep.OutputTokens != tt.output {
				t.Errorf("tokens = %d/%d, want %d/%d", rep.InputTokens, rep.OutputTokens, tt.input, tt.output)
			}
			if rep.TTFT.N != tt.ttftN || rep.Latency.N != tt.latency {
				t.Errorf("ttft n = %d, latency n = %d, want %d, %d", rep.TTFT.N, rep.Latency.N, tt.ttftN, tt.latency)
			}
		})
	}
}

func TestNonStreamBadBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	t.Cleanup(srv.Close)
	if rep := runJSON(t, "-url", srv.URL, "-requests", "1", "-stream=false"); rep.Outcomes[outcomeBadBody] != 1 {
		t.Fatalf("outcomes = %v", rep.Outcomes)
	}
}

func TestTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if rep := runJSON(t, "-url", srv.URL, "-requests", "2"); rep.Outcomes[outcomeTransport] != 2 {
		t.Fatalf("outcomes = %v", rep.Outcomes)
	}
}

func TestTimeoutIsCanceled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	if rep := runJSON(t, "-url", srv.URL, "-requests", "1", "-timeout", "20ms"); rep.Outcomes[outcomeCanceled] != 1 {
		t.Fatalf("outcomes = %v", rep.Outcomes)
	}
}

// countingServer replies ok to every non-stream request and counts the
// requests per server.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		_, _ = io.WriteString(w, `{"usage":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestRequestCountAndRoundRobin(t *testing.T) {
	a, na := countingServer(t)
	b, nb := countingServer(t)
	rep := runJSON(t, "-url", a.URL+","+b.URL, "-requests", "7", "-concurrency", "3", "-stream=false")
	if rep.Requests != 7 || rep.Outcomes[outcomeOK] != 7 {
		t.Fatalf("report = %+v", rep)
	}
	if na.Load() != 4 || nb.Load() != 3 {
		t.Errorf("requests per gateway = %d, %d, want 4, 3", na.Load(), nb.Load())
	}
}

func TestDurationStopsRun(t *testing.T) {
	srv, n := countingServer(t)
	rep := runJSON(t, "-url", srv.URL, "-requests", "0", "-duration", "50ms", "-concurrency", "2", "-stream=false")
	if rep.Requests == 0 || int64(rep.Requests) != n.Load() {
		t.Fatalf("requests = %d, server saw %d", rep.Requests, n.Load())
	}
}

func TestCanceledRunStops(t *testing.T) {
	srv, _ := countingServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := run(ctx, []string{"-url", srv.URL, "-requests", "0", "-duration", "1h", "-json"}, env(testKey), &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"requests":0`) {
		t.Errorf("report = %s, want 0 requests", out.String())
	}
}

func TestQuantiles(t *testing.T) {
	var ds []time.Duration
	for i := 100; i >= 1; i-- {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	if got, want := quantilesOf(ds), (quantiles{N: 100, P50: 50, P95: 95, P99: 99, Max: 100}); got != want {
		t.Errorf("quantilesOf(1..100ms) = %+v, want %+v", got, want)
	}
	three := []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}
	if got, want := quantilesOf(three), (quantiles{N: 3, P50: 2, P95: 3, P99: 3, Max: 3}); got != want {
		t.Errorf("quantilesOf(1..3ms) = %+v, want %+v", got, want)
	}
	one := []time.Duration{3 * time.Millisecond}
	if got, want := quantilesOf(one), (quantiles{N: 1, P50: 3, P95: 3, P99: 3, Max: 3}); got != want {
		t.Errorf("quantilesOf(3ms) = %+v, want %+v", got, want)
	}
	if got := quantilesOf(nil); got != (quantiles{}) {
		t.Errorf("quantilesOf(nil) = %+v", got)
	}
}

func TestTextReport(t *testing.T) {
	rep := summarize([]result{
		{outcome: outcomeOK, latency: 10 * time.Millisecond, ttft: 2 * time.Millisecond},
		{outcome: "http_429"},
		{outcome: "http_429"},
	}, 2*time.Second)
	var out bytes.Buffer
	if err := rep.writeText(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"requests  3 in 2.0s (1.5/s)",
		"outcomes  http_429=2 ok=1",
		"ttft      n=1 p50=2ms",
		"latency   n=1 p50=10ms",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report %q lacks %q", out.String(), want)
		}
	}
}

// TestDocsMatchFlags checks the flag table of docs/loadgen.md against the
// flag set, names and defaults, so that a changed flag fails until the docs
// follow.
func TestDocsMatchFlags(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "loadgen.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]string{}
	for _, m := range regexp.MustCompile("(?m)^\\| `-([a-z-]+)` \\| `?([^`|]*?)`? \\|").FindAllSubmatch(doc, -1) {
		documented[string(m[1])] = string(m[2])
	}
	live := map[string]string{}
	flagSet(&config{}, new(string)).VisitAll(func(f *flag.Flag) {
		def := f.DefValue
		if def == "false" {
			def = "off"
		}
		if d, err := time.ParseDuration(def); err == nil && d > 0 {
			def = strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
		}
		live[f.Name] = def
	})
	if !maps.Equal(documented, live) {
		t.Fatalf("flags in docs/loadgen.md = %v, flag set = %v", documented, live)
	}
}
