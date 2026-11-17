package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/mock"
	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/postgres"
)

const (
	gatewayKey  = "tw_test_cmdgateway"
	upstreamKey = "sk-ant-test-cmdupstream"
	dbPassword  = "tw_test_cmddbpassword"
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

// withDB returns a copy of env with TW_DATABASE_URL set to dbURL.
func withDB(env map[string]string, dbURL string) map[string]string {
	out := maps.Clone(env)
	out["TW_DATABASE_URL"] = dbURL
	return out
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
		is   error
		// db leaves out -no-budgets.
		db bool
	}{
		{"no upstream URL", []string{"-keys", good}, map[string]string{"TW_UPSTREAM_KEY": upstreamKey}, "TW_UPSTREAM_URL is not set", nil, false},
		{"no upstream key", []string{"-keys", good}, map[string]string{"TW_UPSTREAM_URL": "http://127.0.0.1:8081"}, "TW_UPSTREAM_KEY is not set", nil, false},
		{"bad upstream URL", []string{"-keys", good}, map[string]string{"TW_UPSTREAM_URL": "127.0.0.1:8081", "TW_UPSTREAM_KEY": upstreamKey}, "upstream URL", nil, false},
		{"no keys flag", nil, env, "-keys or -db-keys is required", nil, false},
		{"both key sources", []string{"-keys", good, "-db-keys"}, withDB(env, "postgres://tw:"+dbPassword+"@127.0.0.1:1/tw"), "-keys and -db-keys are both set", nil, true},
		{"db keys without database", []string{"-db-keys"}, env, "-db-keys reads the keys from it", nil, false},
		{"db keys, database down", []string{"-db-keys"}, withDB(env, "postgres://tw:"+dbPassword+"@127.0.0.1:1/tw?connect_timeout=5"), "database: ", nil, false},
		{"missing key file", []string{"-keys", filepath.Join(t.TempDir(), "none.json")}, env, "-keys", fs.ErrNotExist, false},
		{"bad JSON", []string{"-keys", writeFile(t, `{"keys":[`)}, env, "key file", nil, false},
		{"unknown field", []string{"-keys", writeFile(t, `{"keys":[{"key":"tw_test_raw","team":"red"}]}`)}, env, "unknown field", nil, false},
		{"zero keys", []string{"-keys", writeFile(t, `{"keys":[]}`)}, env, "holds no keys", nil, false},
		{"addr without port", []string{"-keys", good, "-addr", "127.0.0.1"}, env, "-addr", nil, false},
		{"addr port not a number", []string{"-keys", good, "-addr", "127.0.0.1:http"}, env, "must be a number", nil, false},
		{"metrics port too large", []string{"-keys", good, "-metrics-addr", "127.0.0.1:65536"}, env, "must be a number", nil, false},
		{"same port", []string{"-keys", good, "-addr", "0.0.0.0:9000", "-metrics-addr", "127.0.0.1:9000"}, env, "port other than -addr", nil, false},
		{"unknown flag", []string{"-keys", good, "-bogus"}, env, "flag provided but not defined: -bogus", errUsage, false},
		{"extra argument", []string{"-keys", good, "serve"}, env, `unexpected argument "serve"`, errUsage, false},
		{"no database", []string{"-keys", good}, env, "TW_DATABASE_URL is not set", nil, true},
		{"database and no-budgets", []string{"-keys", good, "-no-budgets"}, withDB(env, "postgres://tw:"+dbPassword+"@127.0.0.1:1/tw"), "both set", nil, true},
		{"bad database URL", []string{"-keys", good}, withDB(env, "postgres://tw:"+dbPassword+"@[::1/tw"), "not a valid Postgres connection string", nil, true},
		{"database down", []string{"-keys", good}, withDB(env, "postgres://tw:"+dbPassword+"@127.0.0.1:1/tw?connect_timeout=5"), "database: ", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if !tt.db {
				args = append([]string{"-no-budgets"}, args...)
			}
			var stderr bytes.Buffer
			cfg, err := parseConfig(args, envOf(tt.env), &stderr)
			if err == nil {
				_, _, _, err = handlers(t.Context(), cfg, slog.New(slog.DiscardHandler))
			}
			if err == nil {
				t.Fatal("error = nil")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("error = %v, want %v", err, tt.is)
			}
			// A usage error is printed to stderr once; main does not print it again.
			text := err.Error()
			if errors.Is(err, errUsage) {
				text = stderr.String()
				if n := strings.Count(text, tt.want); n != 1 {
					t.Fatalf("stderr holds %q %d times:\n%s", tt.want, n, text)
				}
			}
			if !strings.Contains(text, tt.want) {
				t.Fatalf("error = %v, stderr = %q, want %q", err, stderr.String(), tt.want)
			}
			for _, secret := range []string{upstreamKey, dbPassword} {
				if strings.Contains(err.Error()+stderr.String(), secret) {
					t.Fatalf("error holds a secret: %v", err)
				}
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

// startGateway starts the gateway with a key file. An empty dbURL runs it
// with -no-budgets.
func startGateway(t *testing.T, dbURL string) *gatewayUnderTest {
	t.Helper()
	args := []string{"-keys", keyFile(t)}
	if dbURL == "" {
		args = append(args, "-no-budgets")
	}
	return startGatewayArgs(t, args, dbURL)
}

// startGatewayArgs starts the gateway with args. A non-empty dbURL goes in
// TW_DATABASE_URL.
func startGatewayArgs(t *testing.T, args []string, dbURL string) *gatewayUnderTest {
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
	if dbURL != "" {
		env = withDB(env, dbURL)
	}
	cfg, err := parseConfig(args, envOf(env), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	proxy, metrics, closeDB, err := handlers(t.Context(), cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return &gatewayUnderTest{proxy: srv, metrics: metrics, logs: logs, upstream: upstream}
}

// send posts one Messages API request with max_tokens 32 and returns the
// status and body.
func (g *gatewayUnderTest) send(t *testing.T, stream bool) (int, string) {
	t.Helper()
	code, _, body := g.sendMax(t, stream, 32)
	return code, body
}

// sendMax posts one Messages API request and returns the status, the
// response header, and the body.
func (g *gatewayUnderTest) sendMax(t *testing.T, stream bool, maxTokens int) (int, http.Header, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      "claude-test",
		"max_tokens": maxTokens,
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
	return res.StatusCode, res.Header, string(b)
}

// fault sets a fault on the mock upstream.
func (g *gatewayUnderTest) fault(t *testing.T, f mock.Fault) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, g.upstream.URL+"/_faults", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", upstreamKey)
	req.Header.Set("content-type", "application/json")
	res, err := g.upstream.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		t.Fatalf("POST /_faults = %d", res.StatusCode)
	}
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
	g := startGateway(t, "")

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
	g := startBudgetGateway(t)
	if code, body := g.send(t, true); code != http.StatusOK {
		t.Fatalf("stream reply = %d:\n%s", code, body)
	}
	// A CounterVec with no series is not exported, so make the budget
	// metrics count.
	exceedBudget(t, g)
	g.proxy.Close()

	live := uniqueMatches(regexp.MustCompile(`(?m)^# TYPE (tokenwarden_\w+) `), g.scrape(t))
	documented := uniqueMatches(regexp.MustCompile("`(tokenwarden_[a-z_]+)`"), doc)
	if !slices.Equal(live, documented) {
		t.Fatalf("metrics in docs/tokenwarden.md = %v, live metrics = %v", documented, live)
	}
}

// startBudgetGateway starts the gateway on a new, empty database schema,
// which the gateway must migrate, and then gives team red a budget of 600
// tokens per hour.
func startBudgetGateway(t *testing.T) *gatewayUnderTest {
	t.Helper()
	dbURL := pgtest.URL(t)
	g := startGateway(t, dbURL)
	pool, err := pgxpool.New(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := postgres.New(pool).SetBudget(t.Context(), "red", limit.Budget{Tokens: 600, Window: time.Hour}); err != nil {
		t.Fatalf("SetBudget after the gateway started: %v", err)
	}
	return g
}

// exceedBudget runs team red, with a budget of 600 tokens, past its
// budget twice: a long stream that a top-up cuts, and a request that the
// first reserve refuses.
func exceedBudget(t *testing.T, g *gatewayUnderTest) {
	t.Helper()
	// The first hold is the input guess plus 512 output tokens, which
	// fits in 600; the second 512 does not.
	g.fault(t, mock.Fault{LongReply: true, Count: 1})
	code, _, body := g.sendMax(t, true, 2000)
	if code != http.StatusOK || !strings.Contains(body, "event: error") ||
		!strings.Contains(body, `"rate_limit_error"`) || strings.Contains(body, "event: message_stop") {
		t.Fatalf("long stream = %d, want 200 cut by a rate_limit_error event:\n%s", code, body)
	}
	code, header, body := g.sendMax(t, false, 1000)
	if code != http.StatusTooManyRequests || !strings.Contains(body, `"rate_limit_error"`) {
		t.Fatalf("over-budget request = %d, want 429 rate_limit_error:\n%s", code, body)
	}
	if ra, err := strconv.Atoi(header.Get("Retry-After")); err != nil || ra < 1 || ra > 3600 {
		t.Fatalf("Retry-After = %q, want 1 to 3600 seconds", header.Get("Retry-After"))
	}
}

func TestBudgets(t *testing.T) {
	g := startBudgetGateway(t)
	if code, body := g.send(t, false); code != http.StatusOK {
		t.Fatalf("in-budget request = %d:\n%s", code, body)
	}
	exceedBudget(t, g)
	g.proxy.Close()

	text := g.scrape(t)
	for _, want := range []string{
		`tokenwarden_budget_denials_total{stage="reserve",team="red"} 1`,
		`tokenwarden_budget_denials_total{stage="topup",team="red"} 1`,
		`tokenwarden_requests_total{outcome="budget_exceeded",stream="false"} 1`,
		`tokenwarden_topups_total{result="exceeded"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(g.logs.String(), dbPassword) {
		t.Error("logs hold the database password")
	}
}

// TestDBKeys runs the gateway with -db-keys, with and without budgets: it
// starts with no keys, and a key added, revoked, or unknown takes effect on
// the next request.
func TestDBKeys(t *testing.T) {
	for _, noBudgets := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-budgets=%v", noBudgets), func(t *testing.T) {
			dbURL := pgtest.URL(t)
			args := []string{"-db-keys"}
			if noBudgets {
				args = append(args, "-no-budgets")
			}
			g := startGatewayArgs(t, args, dbURL)
			if !strings.Contains(g.logs.String(), "the database holds no active gateway keys") {
				t.Errorf("logs lack the no-keys warning:\n%s", g.logs.String())
			}
			if code, _ := g.send(t, false); code != http.StatusUnauthorized {
				t.Fatalf("before any key = %d, want 401", code)
			}

			pool, err := pgxpool.New(t.Context(), dbURL)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec("INSERT INTO gateway_keys (sha256, team, user_name) VALUES ($1, 'red', 'alice')", auth.HashKey(gatewayKey))
			if !noBudgets {
				if err := postgres.New(pool).SetBudget(t.Context(), "red", limit.Budget{Tokens: 600, Window: time.Hour}); err != nil {
					t.Fatal(err)
				}
			}
			if code, body := g.send(t, false); code != http.StatusOK {
				t.Fatalf("with a key = %d:\n%s", code, body)
			}
			exec("UPDATE gateway_keys SET revoked_at = now()")
			if code, _ := g.send(t, false); code != http.StatusUnauthorized {
				t.Fatalf("after revoke = %d, want 401", code)
			}
			logs := g.logs.String()
			for _, secret := range []string{gatewayKey, auth.HashKey(gatewayKey), dbPassword} {
				if strings.Contains(logs, secret) {
					t.Errorf("logs hold %q", secret)
				}
			}
		})
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
	args := []string{"-no-budgets", "-keys", keyFile(t), "-addr", "127.0.0.1:0", "-metrics-addr", "127.0.0.1:0"}
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
	// Find a free port for the proxy, so that the test can check that run
	// releases it.
	free, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TW_UPSTREAM_URL": "http://127.0.0.1:8081", "TW_UPSTREAM_KEY": upstreamKey}
	args := []string{"-no-budgets", "-keys", keyFile(t), "-addr", proxyAddr, "-metrics-addr", busy.Addr().String()}
	if err := run(t.Context(), args, envOf(env), io.Discard); err == nil {
		t.Fatal("run on a busy metrics port = nil, want an error")
	}
	again, err := lc.Listen(t.Context(), "tcp", proxyAddr)
	if err != nil {
		t.Fatalf("run did not release the proxy port: %v", err)
	}
	again.Close()
}

// TestServeWaitsForServeStartedAfterShutdown holds each Serve call until
// Shutdown has begun, so Shutdown has no listener to close. serve must not
// return before those Serve calls end and close their listeners.
func TestServeWaitsForServeStartedAfterShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var all []served
		// The hooks go in before serve starts; a hook registered after
		// Shutdown never runs.
		shutdownStarted := map[*http.Server]chan struct{}{}
		for _, name := range []string{"proxy", "metrics"} {
			srv := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
			started := make(chan struct{})
			srv.RegisterOnShutdown(func() { close(started) })
			shutdownStarted[srv] = started
			all = append(all, served{name, srv, newPipeListener()})
		}
		release := make(chan struct{})
		orig := serveHTTP
		t.Cleanup(func() { serveHTTP = orig })
		serveHTTP = func(srv *http.Server, ln net.Listener) error {
			<-shutdownStarted[srv]
			<-release
			return srv.Serve(ln)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		errc := make(chan error, 1)
		go func() { errc <- serve(ctx, slog.New(slog.DiscardHandler), all) }()
		synctest.Wait()
		select {
		case err := <-errc:
			close(release)
			t.Fatalf("serve returned (%v) before its Serve calls ended", err)
		default:
		}
		close(release)
		if err := <-errc; err != nil {
			t.Fatalf("serve = %v", err)
		}
		for _, s := range all {
			select {
			case <-s.ln.(*pipeListener).done:
			default:
				t.Errorf("%s listener still open after serve returned", s.name)
			}
		}
	})
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
	go func() {
		errc <- serve(ctx, slog.New(slog.DiscardHandler), []served{{"proxy", proxySrv, lns[0]}, {"metrics", metricsSrv, lns[1]}})
	}()
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
		logs := &syncBuffer{}
		log := slog.New(slog.NewJSONHandler(logs, nil))
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() {
			errc <- serve(ctx, log, []served{{"proxy", proxySrv, pl}, {"metrics", metricsSrv, newPipeListener()}})
		}()

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
		if err != nil {
			t.Fatalf("serve = %v, want nil: a cut at the end of the grace period is normal", err)
		}
		<-cut
		var warns []string
		for line := range strings.Lines(logs.String()) {
			var rec struct{ Level, Msg, Server string }
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line is not JSON: %q", line)
			}
			warns = append(warns, rec.Level+"/"+rec.Server+"/"+rec.Msg)
		}
		if want := []string{"WARN/proxy/shutdown grace period ended; open requests cut"}; !slices.Equal(warns, want) {
			t.Fatalf("logs = %v, want %v", warns, want)
		}
	})
}
