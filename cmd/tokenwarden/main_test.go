package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/mock"
)

const (
	gatewayKey  = "tw_test_cmdgateway"
	upstreamKey = "sk-ant-test-cmdupstream"
)

// writeFile writes data to a new file in a test directory and returns its
// path.
func writeFile(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func keyFile(t *testing.T) string {
	t.Helper()
	return writeFile(t, `{"keys":[{"sha256":"`+auth.HashKey(gatewayKey)+`","team":"red","user":"alice"}]}`)
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// syncBuffer is a bytes.Buffer that is safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestConfigErrors(t *testing.T) {
	good := keyFile(t)
	env := map[string]string{"TW_UPSTREAM_URL": "http://127.0.0.1:8081", "TW_UPSTREAM_KEY": upstreamKey}
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no upstream URL", []string{"-keys", good}, map[string]string{"TW_UPSTREAM_KEY": upstreamKey}, "TW_UPSTREAM_URL is not set"},
		{"no upstream key", []string{"-keys", good}, map[string]string{"TW_UPSTREAM_URL": "http://127.0.0.1:8081"}, "TW_UPSTREAM_KEY is not set"},
		{"bad upstream URL", []string{"-keys", good}, map[string]string{"TW_UPSTREAM_URL": "127.0.0.1:8081", "TW_UPSTREAM_KEY": upstreamKey}, "upstream URL"},
		{"no keys flag", nil, env, "-keys is required"},
		{"missing key file", []string{"-keys", filepath.Join(t.TempDir(), "none.json")}, env, "no such file"},
		{"bad JSON", []string{"-keys", writeFile(t, `{"keys":[`)}, env, "key file"},
		{"unknown field", []string{"-keys", writeFile(t, `{"keys":[{"key":"tw_test_raw","team":"red"}]}`)}, env, "unknown field"},
		{"zero keys", []string{"-keys", writeFile(t, `{"keys":[]}`)}, env, "holds no keys"},
		{"addr without port", []string{"-keys", good, "-addr", "127.0.0.1"}, env, "-addr"},
		{"addr port not a number", []string{"-keys", good, "-addr", "127.0.0.1:http"}, env, "must be a number"},
		{"metrics port too large", []string{"-keys", good, "-metrics-addr", "127.0.0.1:65536"}, env, "must be a number"},
		{"same port", []string{"-keys", good, "-addr", "0.0.0.0:9000", "-metrics-addr", "127.0.0.1:9000"}, env, "port other than -addr"},
		{"extra argument", []string{"-keys", good, "serve"}, env, "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			cfg, err := parseConfig(tt.args, envOf(tt.env), &stderr)
			if err == nil {
				_, _, err = handlers(cfg, slog.New(slog.DiscardHandler))
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), upstreamKey) {
				t.Fatalf("error holds the upstream key: %v", err)
			}
		})
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(t.Context(), []string{"-h"}, envOf(nil), &stderr); err != nil {
		t.Fatalf("run -h = %v", err)
	}
	if !strings.Contains(stderr.String(), "-metrics-addr") {
		t.Fatalf("usage does not list the flags:\n%s", stderr.String())
	}
}

// gatewayUnderTest is the binary's handlers in front of the mock upstream.
type gatewayUnderTest struct {
	proxy    *httptest.Server
	metrics  http.Handler
	logs     *syncBuffer
	upstream *httptest.Server
}

func startGateway(t *testing.T) *gatewayUnderTest {
	t.Helper()
	m := mock.New(mock.Config{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != upstreamKey {
			http.Error(w, "wrong upstream key", http.StatusUnauthorized)
			return
		}
		m.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	env := map[string]string{"TW_UPSTREAM_URL": upstream.URL, "TW_UPSTREAM_KEY": upstreamKey}
	cfg, err := parseConfig([]string{"-keys", keyFile(t)}, envOf(env), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	proxy, metrics, err := handlers(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return &gatewayUnderTest{proxy: srv, metrics: metrics, logs: logs, upstream: upstream}
}

// send posts one Messages API request and returns the status and body.
func (g *gatewayUnderTest) send(t *testing.T, stream bool) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      "claude-test",
		"max_tokens": 32,
		"stream":     stream,
		"messages":   []map[string]string{{"role": "user", "content": "hello gateway"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, g.proxy.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", gatewayKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	res, err := g.proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

// get sends GET path to the proxy and returns the status and body.
func (g *gatewayUnderTest) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.proxy.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

// scrape returns the text of GET /metrics on the metrics handler.
func (g *gatewayUnderTest) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	g.metrics.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestEndToEnd(t *testing.T) {
	g := startGateway(t)

	code, body := g.send(t, true)
	if code != http.StatusOK || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("stream reply = %d:\n%s", code, body)
	}
	code, body = g.send(t, false)
	if code != http.StatusOK || !strings.Contains(body, `"type":"message"`) {
		t.Fatalf("non-stream reply = %d:\n%s", code, body)
	}

	if code, body := g.get(t, "/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("GET /healthz = %d %q", code, body)
	}
	if code, _ := g.get(t, "/metrics"); code != http.StatusNotFound {
		t.Fatalf("GET /metrics on the proxy = %d, want 404", code)
	}

	// Close waits for the handlers, so the audit lines are written.
	g.proxy.Close()

	text := g.scrape(t)
	for _, want := range []string{
		`tokenwarden_requests_total{outcome="ok",stream="true"} 1`,
		`tokenwarden_requests_total{outcome="ok",stream="false"} 1`,
		`tokenwarden_tokens_total{kind="output",team="red"}`,
		"go_goroutines ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	logs := g.logs.String()
	for _, secret := range []string{gatewayKey, upstreamKey, "hello gateway"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs hold %q", secret)
		}
	}
	var audits []string
	for line := range strings.Lines(logs) {
		var rec struct {
			Msg     string `json:"msg"`
			Outcome string `json:"outcome"`
			Team    string `json:"team"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec.Msg == "audit" {
			audits = append(audits, rec.Outcome+"/"+rec.Team)
		}
	}
	if want := []string{"ok/red", "ok/red"}; !slices.Equal(audits, want) {
		t.Fatalf("audit lines = %v, want %v", audits, want)
	}
}

// TestDocsMatchMetrics checks docs/tokenwarden.md against the live metrics,
// so a renamed, added, or removed metric fails until the docs follow.
func TestDocsMatchMetrics(t *testing.T) {
	doc := readDoc(t)
	g := startGateway(t)
	if code, body := g.send(t, true); code != http.StatusOK {
		t.Fatalf("stream reply = %d:\n%s", code, body)
	}
	g.proxy.Close()

	live := uniqueMatches(regexp.MustCompile(`(?m)^# TYPE (tokenwarden_\w+) `), g.scrape(t))
	documented := uniqueMatches(regexp.MustCompile("`(tokenwarden_[a-z_]+)`"), doc)
	if !slices.Equal(live, documented) {
		t.Fatalf("metrics in docs/tokenwarden.md = %v, live metrics = %v", documented, live)
	}
}

// TestDocsKeyFile checks that the example in docs/tokenwarden.md loads and
// holds the hash of the example key.
func TestDocsKeyFile(t *testing.T) {
	doc := readDoc(t)
	m := regexp.MustCompile("(?s)```json\n(.*?)```").FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("docs/tokenwarden.md has no json example")
	}
	s, err := auth.LoadStatic(strings.NewReader(m[1]))
	if err != nil {
		t.Fatalf("example key file: %v", err)
	}
	id, err := s.Authenticate(t.Context(), "tw_test_example")
	if err != nil {
		t.Fatalf("example key file does not hold the hash of tw_test_example: %v", err)
	}
	if id.Team != "red" {
		t.Fatalf("team = %q", id.Team)
	}
}

func readDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "tokenwarden.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func uniqueMatches(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	env := map[string]string{"TW_UPSTREAM_URL": "http://127.0.0.1:8081", "TW_UPSTREAM_KEY": upstreamKey}
	args := []string{"-keys", keyFile(t), "-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0"}
	var stderr syncBuffer
	if err := run(ctx, args, envOf(env), &stderr); err != nil {
		t.Fatalf("run = %v", err)
	}
	if !strings.Contains(stderr.String(), `"msg":"listening"`) {
		t.Fatalf("no listening log:\n%s", stderr.String())
	}
}

func TestRunFailsOnBusyPort(t *testing.T) {
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	env := map[string]string{"TW_UPSTREAM_URL": "http://127.0.0.1:8081", "TW_UPSTREAM_KEY": upstreamKey}
	args := []string{"-keys", keyFile(t), "-addr", "127.0.0.1:0", "-metrics-addr", busy.Addr().String()}
	if err := run(t.Context(), args, envOf(env), io.Discard); err == nil {
		t.Fatal("run on a busy metrics port = nil, want an error")
	}
}

func TestServeShutsDownBothListeners(t *testing.T) {
	var (
		lc  net.ListenConfig
		d   net.Dialer
		lns []net.Listener
	)
	for range 2 {
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
	}
	proxySrv, metricsSrv := servers(http.NotFoundHandler(), http.NotFoundHandler())
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- serve(ctx, []served{{proxySrv, lns[0]}, {metricsSrv, lns[1]}}) }()
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("serve = %v", err)
	}
	for _, ln := range lns {
		if c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String()); err == nil {
			c.Close()
			t.Fatalf("%s still accepts connections", ln.Addr())
		}
	}
}

// pipeListener is a net.Listener of in-memory net.Pipe connections, so that
// a server can run inside a synctest bubble.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial() net.Conn {
	server, client := net.Pipe()
	l.conns <- server
	return client
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func TestServeCutsStreamsAfterGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		cut := make(chan struct{})
		// The handler writes each second, so a write deadline on the proxy
		// server would also cut it early.
		stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(cut)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(started)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-tick.C:
				}
				if _, err := io.WriteString(w, "event: ping\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		})
		proxySrv, metricsSrv := servers(stream, http.NotFoundHandler())
		pl := newPipeListener()
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { errc <- serve(ctx, []served{{proxySrv, pl}, {metricsSrv, newPipeListener()}}) }()

		c := pl.dial()
		defer c.Close()
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: tokenwarden\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		// net.Pipe has no buffer: drain the stream so that the server's
		// writes do not block.
		body := res.Body
		go func() { _, _ = io.Copy(io.Discard, body) }()
		<-started

		start := time.Now()
		cancel()
		time.Sleep(shutdownGrace - time.Millisecond)
		synctest.Wait()
		select {
		case <-cut:
			t.Fatal("the stream was cut before the grace period ended")
		case err := <-errc:
			t.Fatalf("serve returned %v before the grace period ended", err)
		default:
		}

		err = <-errc
		if elapsed := time.Since(start); elapsed != shutdownGrace {
			t.Fatalf("serve returned after %v, want %v", elapsed, shutdownGrace)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve = %v, want context.DeadlineExceeded", err)
		}
		<-cut
	})
}
