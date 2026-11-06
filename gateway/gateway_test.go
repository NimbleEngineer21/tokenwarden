package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
	"github.com/NimbleEngineer21/tokenwarden/internal/mock"
)

const (
	upstreamKey = "sk-ant-test-upstream"
	gatewayKey  = "tw_test_alice"
	textBody    = `{"model":"claude-test","max_tokens":40,"messages":[{"role":"user","content":"hello there"}]}`
	streamBody  = `{"model":"claude-test","max_tokens":40,"stream":true,"messages":[{"role":"user","content":"hello there"}]}`
)

func newAuth(t *testing.T) *auth.Static {
	t.Helper()
	s, err := auth.NewStatic([]auth.StaticKey{{SHA256: auth.HashKey(gatewayKey), Team: "red", User: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// spy sits in front of the mock upstream. It records the headers of each
// /v1/messages request, and it removes x-api-key unless the key is the
// upstream key, so that the mock answers 401 to any other key.
type spy struct {
	next http.Handler
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *spy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/messages" {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Clone(r.Context()))
		s.mu.Unlock()
		if r.Header.Get("x-api-key") != upstreamKey {
			r.Header.Del("x-api-key")
		}
	}
	s.next.ServeHTTP(w, r)
}

func (s *spy) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

type stack struct {
	mock    *httptest.Server
	spy     *spy
	gateway *httptest.Server
}

// newStack starts the mock upstream at full speed and a gateway that sends
// key upstream.
func newStack(t *testing.T, key string) *stack {
	t.Helper()
	sp := &spy{next: mock.New(mock.Config{})}
	ms := httptest.NewServer(sp)
	t.Cleanup(ms.Close)
	h, err := New(Config{UpstreamURL: ms.URL, UpstreamKey: key, Authenticator: newAuth(t)})
	if err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(h)
	t.Cleanup(gs.Close)
	return &stack{mock: ms, spy: sp, gateway: gs}
}

func (s *stack) setFault(t *testing.T, fault string) {
	t.Helper()
	resp := do(t, s.mock.URL+"/_faults", fault, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set fault %s: status %d", fault, resp.StatusCode)
	}
}

// reply is a finished HTTP response with its body read.
type reply struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
	Body          []byte
}

// do sends a POST and returns the whole reply.
func do(t *testing.T, url, body string, hdr http.Header) *reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	// No transparent gzip, so that the test sees the encoding on the wire.
	c := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &reply{StatusCode: resp.StatusCode, Header: resp.Header, ContentLength: resp.ContentLength, Body: b}
}

func keyHeader() http.Header {
	return http.Header{"X-Api-Key": {gatewayKey}, "Anthropic-Version": {"2023-06-01"}}
}

func errorBody(t *testing.T, resp *reply) anthropic.ErrorBody {
	t.Helper()
	var e anthropic.ErrorBody
	if err := json.Unmarshal(resp.Body, &e); err != nil {
		t.Fatalf("error body: %v", err)
	}
	return e
}

func TestNewRejectsConfig(t *testing.T) {
	a := newAuth(t)
	ok := Config{UpstreamURL: "http://127.0.0.1:8081", UpstreamKey: upstreamKey, Authenticator: a}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"no upstream", func(c *Config) { c.UpstreamURL = "" }},
		{"relative upstream", func(c *Config) { c.UpstreamURL = "api.anthropic.com" }},
		{"ftp upstream", func(c *Config) { c.UpstreamURL = "ftp://api.anthropic.com" }},
		{"upstream query", func(c *Config) { c.UpstreamURL = "https://api.anthropic.com?x=1" }},
		{"upstream user info", func(c *Config) { c.UpstreamURL = "https://u:p@api.anthropic.com" }},
		{"no key", func(c *Config) { c.UpstreamKey = "" }},
		{"key with newline", func(c *Config) { c.UpstreamKey = upstreamKey + "\n" }},
		{"no authenticator", func(c *Config) { c.Authenticator = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ok
			tt.edit(&c)
			if h, err := New(c); err == nil || h != nil {
				t.Fatalf("New = %v, %v; want nil and an error", h, err)
			}
		})
	}
	if _, err := New(ok); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

func TestKeyInjection(t *testing.T) {
	s := newStack(t, upstreamKey)
	hdr := keyHeader()
	hdr.Set("Authorization", "Bearer "+gatewayKey)
	hdr.Set("Cookie", "session=tw_test_cookie")
	hdr.Set("X-Custom", "tw_test_custom")
	hdr.Set("Anthropic-Beta", "test-beta")
	resp := do(t, s.gateway.URL+"/v1/messages?beta=true", textBody, hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", resp.StatusCode, resp.Body)
	}
	reqs := s.spy.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if k := got.Header.Get("X-Api-Key"); k != upstreamKey {
		t.Errorf("upstream x-api-key = %q, want the upstream key", k)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Custom"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("upstream got %s = %q, want it removed", h, v)
		}
	}
	for h, want := range map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "test-beta"} {
		if v := got.Header.Get(h); v != want {
			t.Errorf("upstream %s = %q, want %q", h, v, want)
		}
	}
	if got.URL.RawQuery != "beta=true" {
		t.Errorf("upstream query = %q, want beta=true", got.URL.RawQuery)
	}
}

func TestUpstreamRejectsGatewayKey(t *testing.T) {
	s := newStack(t, "sk-ant-test-wrong")
	resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	e := errorBody(t, resp)
	if e.Error.Type != anthropic.ErrAPI || e.Error.Message != "upstream rejected the gateway credentials" {
		t.Errorf("error = %+v", e.Error)
	}
	if id := resp.Header.Get(RequestIDHeader); id == "" || e.RequestID != id {
		t.Errorf("body request_id = %q, header = %q; want equal and set", e.RequestID, id)
	}
}

func TestClientAuth(t *testing.T) {
	s := newStack(t, upstreamKey)
	tests := []struct {
		name   string
		hdr    http.Header
		status int
	}{
		{"no key", http.Header{}, http.StatusUnauthorized},
		{"unknown key", http.Header{"X-Api-Key": {"tw_test_unknown"}}, http.StatusUnauthorized},
		{"basic scheme", http.Header{"Authorization": {"Basic " + gatewayKey}}, http.StatusUnauthorized},
		{"bearer", http.Header{"Authorization": {"Bearer " + gatewayKey}}, http.StatusOK},
		{"x-api-key", http.Header{"X-Api-Key": {gatewayKey}}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(s.spy.requests())
			resp := do(t, s.gateway.URL+"/v1/messages", textBody, tt.hdr)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			sent := len(s.spy.requests()) - before
			if tt.status == http.StatusUnauthorized {
				if e := errorBody(t, resp); e.Error.Type != anthropic.ErrAuthentication {
					t.Errorf("error type = %q, want %q", e.Error.Type, anthropic.ErrAuthentication)
				}
				if sent != 0 {
					t.Errorf("upstream got %d requests for a refused key, want 0", sent)
				}
			}
		})
	}
}

func TestStatusFaultsPassThrough(t *testing.T) {
	tests := []struct {
		status  int
		errType string
	}{
		{http.StatusTooManyRequests, anthropic.ErrRateLimit},
		{http.StatusInternalServerError, anthropic.ErrAPI},
		{529, anthropic.ErrOverloaded},
	}
	for _, tt := range tests {
		for _, body := range []string{textBody, streamBody} {
			s := newStack(t, upstreamKey)
			s.setFault(t, fmt.Sprintf(`{"status":%d,"count":1}`, tt.status))
			resp := do(t, s.gateway.URL+"/v1/messages", body, keyHeader())
			if resp.StatusCode != tt.status {
				t.Fatalf("%d: status = %d", tt.status, resp.StatusCode)
			}
			e := errorBody(t, resp)
			if e.Error.Type != tt.errType {
				t.Errorf("%d: error type = %q, want %q", tt.status, e.Error.Type, tt.errType)
			}
			if id := resp.Header.Get("Request-Id"); !strings.HasPrefix(id, "req_mock_") || e.RequestID != id {
				t.Errorf("%d: request-id = %q, body %q; want the upstream ID in both", tt.status, id, e.RequestID)
			}
			wantRetry := ""
			if tt.status == http.StatusTooManyRequests {
				wantRetry = "1"
			}
			if got := resp.Header.Get("Retry-After"); got != wantRetry {
				t.Errorf("%d: retry-after = %q, want %q", tt.status, got, wantRetry)
			}
		}
	}
}

func TestNonStreamUnchanged(t *testing.T) {
	s := newStack(t, upstreamKey)
	direct := do(t, s.mock.URL+"/v1/messages", textBody, http.Header{"X-Api-Key": {upstreamKey}})
	resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	want, got := direct.Body, resp.Body
	if !bytes.Equal(got, want) {
		t.Fatalf("body differs from the upstream reply:\n got %s\nwant %s", got, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if resp.ContentLength != int64(len(want)) {
		t.Errorf("content-length = %d, want %d", resp.ContentLength, len(want))
	}
}

func TestUnknownRoute(t *testing.T) {
	s := newStack(t, upstreamKey)
	resp := do(t, s.gateway.URL+"/v1/models", "", keyHeader())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if e := errorBody(t, resp); e.Error.Type != anthropic.ErrNotFound {
		t.Errorf("error type = %q", e.Error.Type)
	}
}

func TestUpstreamDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	h, err := New(Config{UpstreamURL: url, UpstreamKey: upstreamKey, Authenticator: newAuth(t)})
	if err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(h)
	defer gs.Close()
	resp := do(t, gs.URL+"/v1/messages", textBody, keyHeader())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if e := errorBody(t, resp); e.Error.Type != anthropic.ErrAPI {
		t.Errorf("error type = %q", e.Error.Type)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var hits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	h, err := New(Config{UpstreamURL: redir.URL, UpstreamKey: upstreamKey, Authenticator: newAuth(t)})
	if err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(h)
	defer gs.Close()
	resp := do(t, gs.URL+"/v1/messages", textBody, keyHeader())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("redirect target got %d requests, want 0", hits)
	}
}
