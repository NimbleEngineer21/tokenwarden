package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/memory"
)

const budgetWindow = time.Hour

// recLimiter wraps a limiter and records what the gateway asks of it.
type recLimiter struct {
	next limit.Limiter

	mu       sync.Mutex
	reserves []int64
	topUps   []int64
	settles  []int64
}

func (l *recLimiter) Reserve(ctx context.Context, team string, tokens int64) (limit.Reservation, error) {
	l.mu.Lock()
	l.reserves = append(l.reserves, tokens)
	l.mu.Unlock()
	r, err := l.next.Reserve(ctx, team, tokens)
	if err != nil {
		return nil, err
	}
	return &recReservation{Reservation: r, l: l}, nil
}

type recReservation struct {
	limit.Reservation
	l *recLimiter
}

func (r *recReservation) TopUp(ctx context.Context, tokens int64) error {
	r.l.mu.Lock()
	r.l.topUps = append(r.l.topUps, tokens)
	r.l.mu.Unlock()
	return r.Reservation.TopUp(ctx, tokens)
}

func (r *recReservation) Settle(ctx context.Context, used int64) error {
	r.l.mu.Lock()
	r.l.settles = append(r.l.settles, used)
	r.l.mu.Unlock()
	return r.Reservation.Settle(ctx, used)
}

// calls returns copies of what the limiter saw.
func (l *recLimiter) calls() (reserves, topUps, settles []int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.reserves...), append([]int64(nil), l.topUps...), append([]int64(nil), l.settles...)
}

// newBudgetStack starts a stack whose gateway enforces a budget of tokens
// per hour for team red.
func newBudgetStack(t *testing.T, tokens int64, opts ...func(*Config)) (*stack, *recLimiter) {
	t.Helper()
	ml, err := memory.New(map[string]limit.Budget{"red": {Tokens: tokens, Window: budgetWindow}})
	if err != nil {
		t.Fatal(err)
	}
	rl := &recLimiter{next: ml}
	opts = append([]func(*Config){func(c *Config) { c.Limiter = rl }}, opts...)
	return newStack(t, upstreamKey, opts...), rl
}

func inputGuess(body string) int64 {
	return limit.ByteEstimator{}.InputTokens([]byte(body))
}

func usageSum(u audit.Usage) int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

func TestByteEstimator(t *testing.T) {
	for n, want := range map[int64]int64{0: 0, 1: 1, 3: 1, 4: 1, 5: 2, 8: 2, 9: 3} {
		if got := (limit.ByteEstimator{}).OutputTokens(n); got != want {
			t.Errorf("OutputTokens(%d) = %d, want %d", n, got, want)
		}
		if got := (limit.ByteEstimator{}).InputTokens(make([]byte, n)); got != want {
			t.Errorf("InputTokens(%d bytes) = %d, want %d", n, got, want)
		}
	}
}

// TestBudgetFirstReserve checks the first hold: a stream holds one chunk of
// output, a non-stream reply holds all of max_tokens, and neither holds more
// than max_tokens.
func TestBudgetFirstReserve(t *testing.T) {
	big := strings.Replace(streamBody, `"max_tokens":40`, `"max_tokens":1000`, 1)
	bigWhole := strings.Replace(textBody, `"max_tokens":40`, `"max_tokens":1000`, 1)
	tests := []struct {
		name, body string
		want       int64
	}{
		{"stream", big, inputGuess(big) + topUpChunk},
		{"stream below one chunk", streamBody, inputGuess(streamBody) + 40},
		{"non-stream", bigWhole, inputGuess(bigWhole) + 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, rl := newBudgetStack(t, 1_000_000)
			resp := do(t, s.gateway.URL+"/v1/messages", tt.body, keyHeader())
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, resp.Body)
			}
			ev := s.event(t)
			reserves, _, settles := rl.calls()
			if len(reserves) != 1 || reserves[0] != tt.want {
				t.Fatalf("reserves = %v, want [%d]", reserves, tt.want)
			}
			if len(settles) != 1 || settles[0] != usageSum(ev.Usage) || settles[0] == 0 {
				t.Fatalf("settles = %v, want [%d] (the real usage)", settles, usageSum(ev.Usage))
			}
		})
	}
}

func TestBudgetRefusals(t *testing.T) {
	tests := []struct {
		name    string
		tokens  int64
		body    string
		status  int
		errType string
		outcome audit.Outcome
	}{
		{"over budget", 10, textBody, http.StatusTooManyRequests, anthropic.ErrRateLimit, audit.OutcomeBudgetExceeded},
		{"stream over budget", 10, streamBody, http.StatusTooManyRequests, anthropic.ErrRateLimit, audit.OutcomeBudgetExceeded},
		{"no max_tokens", 1000, `{"model":"m","messages":[]}`, http.StatusBadRequest, anthropic.ErrInvalidRequest, audit.OutcomeInvalid},
		{"zero max_tokens", 1000, `{"model":"m","max_tokens":0}`, http.StatusBadRequest, anthropic.ErrInvalidRequest, audit.OutcomeInvalid},
		{"text max_tokens", 1000, `{"model":"m","max_tokens":"9"}`, http.StatusBadRequest, anthropic.ErrInvalidRequest, audit.OutcomeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, rl := newBudgetStack(t, tt.tokens)
			resp := do(t, s.gateway.URL+"/v1/messages", tt.body, keyHeader())
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.status, resp.Body)
			}
			if e := errorBody(t, resp); e.Error.Type != tt.errType || e.RequestID == "" {
				t.Fatalf("error body = %s", resp.Body)
			}
			if ev := s.event(t); ev.Outcome != tt.outcome || ev.ErrorType != tt.errType {
				t.Fatalf("audit outcome %q type %q, want %q %q", ev.Outcome, ev.ErrorType, tt.outcome, tt.errType)
			}
			if n := len(s.spy.requests()); n != 0 {
				t.Fatalf("%d requests went upstream, want none", n)
			}
			if _, _, settles := rl.calls(); len(settles) != 0 {
				t.Fatalf("settles = %v, want none", settles)
			}
			if tt.status != http.StatusTooManyRequests {
				return
			}
			ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err != nil || ra < 1 || ra > int(budgetWindow/time.Second) {
				t.Fatalf("Retry-After = %q, want 1..%d seconds", resp.Header.Get("Retry-After"), int(budgetWindow/time.Second))
			}
			if got := metricValue(t, s.reg, "tokenwarden_budget_denials_total", map[string]string{"team": "red", "stage": "reserve"}); got != 1 {
				t.Fatalf("budget_denials_total{reserve} = %v, want 1", got)
			}
		})
	}
}

func TestBudgetConcurrency(t *testing.T) {
	ml, err := memory.New(map[string]limit.Budget{"red": {Tokens: 1000, Window: budgetWindow, MaxInflight: 1}})
	if err != nil {
		t.Fatal(err)
	}
	held, err := ml.Reserve(t.Context(), "red", 1)
	if err != nil {
		t.Fatal(err)
	}
	s := newStack(t, upstreamKey, func(c *Config) { c.Limiter = ml })
	resp := do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
	if resp.StatusCode != http.StatusTooManyRequests || errorBody(t, resp).Error.Type != anthropic.ErrRateLimit {
		t.Fatalf("status %d body %s; want 429 rate_limit_error", resp.StatusCode, resp.Body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none", ra)
	}
	if ev := s.event(t); ev.Outcome != audit.OutcomeBudgetExceeded {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
	if n := len(s.spy.requests()); n != 0 {
		t.Fatalf("%d requests went upstream, want none", n)
	}
	if got := metricValue(t, s.reg, "tokenwarden_budget_denials_total", map[string]string{"team": "red", "stage": "concurrency"}); got != 1 {
		t.Fatalf("budget_denials_total{concurrency} = %v, want 1", got)
	}

	if err := held.Settle(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader()); resp.StatusCode != http.StatusOK {
		t.Fatalf("after the slot freed: status %d body %s", resp.StatusCode, resp.Body)
	}
}

func TestBudgetNoBudget(t *testing.T) {
	ml, err := memory.New(map[string]limit.Budget{"blue": {Tokens: 1000, Window: budgetWindow}})
	if err != nil {
		t.Fatal(err)
	}
	s := newStack(t, upstreamKey, func(c *Config) { c.Limiter = ml })
	resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader())
	if resp.StatusCode != http.StatusForbidden || errorBody(t, resp).Error.Type != anthropic.ErrPermission {
		t.Fatalf("status %d body %s; want 403 permission_error", resp.StatusCode, resp.Body)
	}
	if ev := s.event(t); ev.Outcome != audit.OutcomeBudgetExceeded {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
}

// failLimiter fails Reserve with reserveErr, or else holds anything and
// fails TopUp with topUpErr.
type failLimiter struct {
	reserveErr, topUpErr error
	settled              chan int64
	topUps               int
}

func (l *failLimiter) Reserve(_ context.Context, _ string, tokens int64) (limit.Reservation, error) {
	if l.reserveErr != nil {
		return nil, l.reserveErr
	}
	return &failReservation{l: l, held: tokens}, nil
}

type failReservation struct {
	l    *failLimiter
	held int64
}

// pipeUpstream returns a transport whose one stream reply carries what the
// test writes to the returned pipe.
func pipeUpstream() (http.RoundTripper, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       pr,
			Request:    r,
		}, nil
	}), pw
}

func (r *failReservation) Window() limit.Window { return limit.Window{} }
func (r *failReservation) Reserved() int64      { return r.held }

func (r *failReservation) TopUp(_ context.Context, tokens int64) error {
	if r.l.topUpErr != nil {
		return r.l.topUpErr
	}
	r.l.topUps++
	r.held += tokens
	return nil
}

func (r *failReservation) Settle(_ context.Context, used int64) error {
	r.l.settled <- used
	return nil
}

func TestBudgetLimiterDown(t *testing.T) {
	down := errors.New("connection refused")
	s := newStack(t, upstreamKey, func(c *Config) { c.Limiter = &failLimiter{reserveErr: down} })
	resp := do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
	if resp.StatusCode != http.StatusServiceUnavailable || errorBody(t, resp).Error.Type != anthropic.ErrAPI {
		t.Fatalf("status %d body %s; want 503 api_error", resp.StatusCode, resp.Body)
	}
	if ev := s.event(t); ev.Outcome != audit.OutcomeGatewayError {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
	if n := len(s.spy.requests()); n != 0 {
		t.Fatalf("%d requests went upstream, want none", n)
	}
}

// longBody asks for a stream of maxTokens; the long_reply fault fills it.
func longBody(maxTokens int) string {
	return strings.Replace(streamBody, `"max_tokens":40`, fmt.Sprintf(`"max_tokens":%d`, maxTokens), 1)
}

// sentBytes returns the generated bytes in the deltas of a stream.
func sentBytes(evs []anthropic.Event) int64 {
	var n int64
	for _, e := range evs {
		if e.Name == anthropic.EventContentBlockDelta {
			n += deltaBytes(e.Data)
		}
	}
	return n
}

// TestBudgetTopUp streams a reply longer than one chunk under a budget that
// has room for all of it. The stream must reach the client unchanged, with
// top-ups of one chunk that stop at input plus max_tokens.
func TestBudgetTopUp(t *testing.T) {
	const maxTokens = 2000
	body := longBody(maxTokens)
	s, rl := newBudgetStack(t, 1_000_000)
	s.setFault(t, `{"long_reply":true}`)
	direct := do(t, s.mock.URL+"/v1/messages", body, http.Header{"X-Api-Key": {upstreamKey}})
	resp := do(t, s.gateway.URL+"/v1/messages", body, keyHeader())
	if !bytes.Equal(resp.Body, direct.Body) {
		t.Fatal("stream with top-ups differs from the upstream stream")
	}
	ev := s.event(t)
	if ev.Outcome != audit.OutcomeOK {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
	reserves, topUps, settles := rl.calls()
	ceiling := inputGuess(body) + maxTokens
	held := reserves[0]
	for i, n := range topUps {
		if n <= 0 || n > topUpChunk {
			t.Fatalf("top-up %d = %d tokens, want 1..%d", i, n, topUpChunk)
		}
		held += n
	}
	if len(topUps) == 0 || held != ceiling {
		t.Fatalf("top-ups %v hold %d in all, want %d (input guess + max_tokens)", topUps, held, ceiling)
	}
	if len(settles) != 1 || settles[0] != usageSum(ev.Usage) {
		t.Fatalf("settles = %v, want [%d]", settles, usageSum(ev.Usage))
	}
	if got := metricValue(t, s.reg, "tokenwarden_topups_total", map[string]string{"result": "ok"}); got != float64(len(topUps)) {
		t.Fatalf("topups_total{ok} = %v, want %d", got, len(topUps))
	}
}

// TestBudgetCutOff gives a stream room for its first chunk and part of a
// second. The gateway must send no text past what it holds, then an error
// event, and charge no more than the budget.
func TestBudgetCutOff(t *testing.T) {
	body := longBody(2000)
	budget := inputGuess(body) + topUpChunk + topUpChunk/2
	s, rl := newBudgetStack(t, budget)
	s.setFault(t, `{"long_reply":true}`)
	resp := do(t, s.gateway.URL+"/v1/messages", body, keyHeader())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	evs := readEvents(t, resp.Body)
	last := evs[len(evs)-1]
	se, err := anthropic.ParseStreamEvent(last.Data)
	if err != nil {
		t.Fatal(err)
	}
	if last.Name != anthropic.EventError || se.Error == nil || se.Error.Type != anthropic.ErrRateLimit || se.Error.Message != errMsgExceeded {
		t.Fatalf("last event = %s %s, want a rate_limit_error event", last.Name, last.Data)
	}
	for _, e := range evs {
		if e.Name == anthropic.EventMessageStop {
			t.Fatal("a cut-off stream has message_stop")
		}
	}
	held := inputGuess(body) + topUpChunk
	if est := inputGuess(body) + (limit.ByteEstimator{}).OutputTokens(sentBytes(evs)); est > held {
		t.Fatalf("client got text worth %d tokens with %d held", est, held)
	}
	ev := s.event(t)
	if ev.Outcome != audit.OutcomeBudgetExceeded || ev.Status != http.StatusOK || ev.ErrorType != anthropic.ErrRateLimit {
		t.Fatalf("audit outcome %q status %d type %q", ev.Outcome, ev.Status, ev.ErrorType)
	}
	_, topUps, settles := rl.calls()
	if len(topUps) != 1 {
		t.Fatalf("top-ups = %v, want one failed top-up", topUps)
	}
	wantUsed := ev.Usage.InputTokens + (limit.ByteEstimator{}).OutputTokens(sentBytes(evs))
	if len(settles) != 1 || settles[0] != wantUsed || settles[0] > budget {
		t.Fatalf("settles = %v, want [%d] and at most the budget %d", settles, wantUsed, budget)
	}
	if got := metricValue(t, s.reg, "tokenwarden_budget_denials_total", map[string]string{"team": "red", "stage": "topup"}); got != 1 {
		t.Fatalf("budget_denials_total{topup} = %v, want 1", got)
	}
	if got := metricValue(t, s.reg, "tokenwarden_topups_total", map[string]string{"result": "exceeded"}); got != 1 {
		t.Fatalf("topups_total{exceeded} = %v, want 1", got)
	}
}

func TestBudgetTopUpFails(t *testing.T) {
	fl := &failLimiter{topUpErr: errors.New("connection refused"), settled: make(chan int64, 1)}
	s := newStack(t, upstreamKey, func(c *Config) { c.Limiter = fl })
	s.setFault(t, `{"long_reply":true}`)
	resp := do(t, s.gateway.URL+"/v1/messages", longBody(2000), keyHeader())
	evs := readEvents(t, resp.Body)
	last := evs[len(evs)-1]
	if se, err := anthropic.ParseStreamEvent(last.Data); err != nil || se.Error == nil || se.Error.Type != anthropic.ErrAPI {
		t.Fatalf("last event = %s %s, want an api_error event", last.Name, last.Data)
	}
	if ev := s.event(t); ev.Outcome != audit.OutcomeGatewayError {
		t.Fatalf("outcome = %q", ev.Outcome)
	}
	if used := <-fl.settled; used <= 0 {
		t.Fatalf("settled %d after text reached the client, want > 0", used)
	}
}

// bigGuess guesses 100 tokens for each output byte.
type bigGuess struct{ limit.ByteEstimator }

func (bigGuess) OutputTokens(n int64) int64 { return n * 100 }

// TestBudgetCeiling checks that a guess past max_tokens neither tops up past
// the ceiling nor cuts the stream off: the upstream cannot send more than
// max_tokens.
func TestBudgetCeiling(t *testing.T) {
	s, rl := newBudgetStack(t, 1_000_000, func(c *Config) { c.Estimator = bigGuess{} })
	resp := do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
	if evs := readEvents(t, resp.Body); evs[len(evs)-1].Name != anthropic.EventMessageStop {
		t.Fatalf("stream ended with %q, want message_stop", evs[len(evs)-1].Name)
	}
	ev := s.event(t)
	_, topUps, settles := rl.calls()
	if len(topUps) != 0 || ev.Outcome != audit.OutcomeOK {
		t.Fatalf("top-ups %v outcome %q; want none and ok", topUps, ev.Outcome)
	}
	if len(settles) != 1 || settles[0] != usageSum(ev.Usage) {
		t.Fatalf("settles = %v, want the real usage %d", settles, usageSum(ev.Usage))
	}
}

// TestBudgetSettle checks what a reply that did not end well costs.
func TestBudgetSettle(t *testing.T) {
	t.Run("upstream error status", func(t *testing.T) {
		s, rl := newBudgetStack(t, 1_000_000)
		s.setFault(t, `{"status":529}`)
		_ = do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
		_ = s.event(t)
		if _, _, settles := rl.calls(); len(settles) != 1 || settles[0] != 0 {
			t.Fatalf("settles = %v, want [0]", settles)
		}
	})
	t.Run("stream dropped", func(t *testing.T) {
		s, rl := newBudgetStack(t, 1_000_000)
		s.setFault(t, `{"drop_after_events":6}`)
		resp := do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
		evs := readEvents(t, resp.Body)
		ev := s.event(t)
		if ev.Outcome != audit.OutcomeIncomplete {
			t.Fatalf("outcome = %q", ev.Outcome)
		}
		want := ev.Usage.InputTokens + max(ev.Usage.OutputTokens, (limit.ByteEstimator{}).OutputTokens(sentBytes(evs)))
		if _, _, settles := rl.calls(); len(settles) != 1 || settles[0] != want || want <= ev.Usage.InputTokens {
			t.Fatalf("settles = %v, want [%d] (real input + guessed output)", settles, want)
		}
	})
}

// TestBudgetStreamFlushes checks that the delta parse and the top-up keep
// each event in its own flush.
func TestBudgetStreamFlushes(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Lorem\"}}\n\n",
		// Past the first chunk: this delta needs a top-up.
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"" + strings.Repeat("ipsum ", 100) + "\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	synctest.Test(t, func(t *testing.T) {
		pr, pw := pipeUpstream()
		fl := &failLimiter{settled: make(chan int64, 1)}
		h, err := New(Config{
			UpstreamURL:   "http://upstream.test",
			UpstreamKey:   upstreamKey,
			Authenticator: newAuth(t),
			Client:        &http.Client{Transport: pr},
			Limiter:       fl,
			Estimator:     perByte{},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := &flushRecorder{header: http.Header{}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(longBody(2000)))
		req.Header.Set("X-Api-Key", gatewayKey)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(rec, req)
		}()
		var want strings.Builder
		for i, e := range events {
			if _, err := pw.Write([]byte(e)); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			want.WriteString(e)
			if got := rec.flushedString(); got != want.String() {
				t.Fatalf("after event %d the client has %q, want %q", i, got, want.String())
			}
		}
		_ = pw.Close()
		<-done
		if fl.topUps == 0 {
			t.Fatal("no top-up ran; the test does not cover the top-up path")
		}
	})
}

// perByte guesses one token for each output byte.
type perByte struct{ limit.ByteEstimator }

func (perByte) OutputTokens(n int64) int64 { return n }
