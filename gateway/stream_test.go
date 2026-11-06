package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// readEvents returns every event of an SSE body.
func readEvents(t *testing.T, body []byte) []anthropic.Event {
	t.Helper()
	rd := anthropic.NewReader(bytes.NewReader(body), anthropic.Limits{})
	var evs []anthropic.Event
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return evs
		}
		if err != nil {
			t.Fatalf("event %d: %v", len(evs), err)
		}
		evs = append(evs, ev)
	}
}

func TestStreamRelayUnchanged(t *testing.T) {
	s := newStack(t, upstreamKey)
	direct := do(t, s.mock.URL+"/v1/messages", streamBody, http.Header{"X-Api-Key": {upstreamKey}})
	hdr := keyHeader()
	hdr.Set("Accept-Encoding", "gzip, br")
	resp := do(t, s.gateway.URL+"/v1/messages", streamBody, hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("content-encoding = %q, want none on text/event-stream", ce)
	}
	if ct := resp.Header.Get("Content-Type"); !isEventStream(ct) {
		t.Errorf("content-type = %q", ct)
	}
	want, got := direct.Body, resp.Body
	if !bytes.Equal(got, want) {
		t.Fatalf("stream differs from the upstream stream:\n got %s\nwant %s", got, want)
	}
	evs := readEvents(t, got)
	if last := evs[len(evs)-1].Name; last != anthropic.EventMessageStop {
		t.Fatalf("last event = %q, want message_stop", last)
	}
}

func TestStreamWithoutMessageStopFails(t *testing.T) {
	const keep = 3
	for _, fault := range []string{
		`{"drop_after_events":3}`,
		`{"drop_after_events":3,"drop_mid_event":true}`,
	} {
		t.Run(fault, func(t *testing.T) {
			s := newStack(t, upstreamKey)
			s.setFault(t, fault)
			resp := do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			evs := readEvents(t, resp.Body)
			if len(evs) != keep+1 {
				t.Fatalf("got %d events, want %d upstream events and one error", len(evs), keep)
			}
			for _, ev := range evs[:keep] {
				if ev.Name == anthropic.EventError || ev.Name == anthropic.EventMessageStop {
					t.Fatalf("relayed event %q before the drop", ev.Name)
				}
			}
			last := evs[keep]
			if last.Name != anthropic.EventError {
				t.Fatalf("last event = %q, want error", last.Name)
			}
			ev, err := anthropic.ParseStreamEvent(last.Data)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Error == nil || ev.Error.Type != anthropic.ErrAPI || ev.Error.Message != errIncomplete {
				t.Fatalf("error event = %s", last.Data)
			}
		})
	}
}

// TestStreamCleanEnd covers an upstream that closes the stream cleanly.
// The mock's drop faults cannot do this: they abort the chunked body, which
// the gateway reads as an unexpected EOF.
func TestStreamCleanEnd(t *testing.T) {
	const start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"
	const upErr = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	tests := []struct {
		name, upstream, want string
	}{
		{"after error event", start + upErr, start + upErr},
		{"without message_stop", start, start + "event: error\ndata: " + string(anthropic.NewErrorBody(anthropic.ErrAPI, errIncomplete, "")) + "\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.upstream)
			}))
			defer us.Close()
			h, err := New(Config{UpstreamURL: us.URL, UpstreamKey: upstreamKey, Authenticator: newAuth(t)})
			if err != nil {
				t.Fatal(err)
			}
			gs := httptest.NewServer(h)
			defer gs.Close()
			resp := do(t, gs.URL+"/v1/messages", streamBody, keyHeader())
			// The gateway puts its request ID in the error event; blank it so
			// that the bytes compare.
			got := strings.Replace(string(resp.Body), `,"request_id":"`+resp.Header.Get(RequestIDHeader)+`"`, "", 1)
			if got != tt.want {
				t.Fatalf("stream = %q, want %q", got, tt.want)
			}
		})
	}
}

// flushRecorder is an http.ResponseWriter that keeps the bytes that have
// been flushed apart from the bytes still pending.
type flushRecorder struct {
	mu      sync.Mutex
	header  http.Header
	pending bytes.Buffer
	flushed bytes.Buffer
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending.Write(b)
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.pending.WriteTo(&f.flushed)
}

func (f *flushRecorder) flushedString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushed.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// TestStreamFlushesEachEvent sends the upstream events one at a time and
// checks, before it sends the next one, that the client has received each
// event in a flush. The upstream body is an io.Pipe, so the bubble's
// goroutines block durably and synctest.Wait knows when the gateway is idle.
func TestStreamFlushesEachEvent(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n",
		": a comment line\nevent: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Lorem\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	synctest.Test(t, func(t *testing.T) {
		pr, pw := io.Pipe()
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       pr,
				Request:    r,
			}, nil
		})
		h, err := New(Config{
			UpstreamURL:   "http://upstream.test",
			UpstreamKey:   upstreamKey,
			Authenticator: newAuth(t),
			Client:        &http.Client{Transport: rt},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := &flushRecorder{header: http.Header{}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(streamBody))
		req.Header.Set("X-Api-Key", gatewayKey)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(rec, req)
		}()

		var want strings.Builder
		for i, ev := range events {
			if _, err := io.WriteString(pw, ev); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			want.WriteString(ev)
			if got := rec.flushedString(); got != want.String() {
				t.Fatalf("after event %d the client has %q, want %q", i, got, want.String())
			}
		}
		_ = pw.Close()
		<-done
		if got := rec.flushedString(); got != want.String() {
			t.Fatalf("final stream = %q, want %q", got, want.String())
		}
	})
}
