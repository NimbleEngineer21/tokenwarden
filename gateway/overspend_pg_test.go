package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
	"github.com/NimbleEngineer21/tokenwarden/internal/mock"
	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/postgres"
)

// Load of TestOverspendBound.
const (
	overspendBudget    = 50_000
	overspendWorkers   = 32
	overspendMaxTokens = 4096 // eight chunks, so a stream must top up
	// overspendPerWorker stops a worker that is never refused, as with a
	// broken Reserve, so that the test ends with a count rather than a
	// timeout.
	overspendPerWorker = 40
)

// outcome is what a client saw for one request.
type outcome int

const (
	outcomeOK      outcome = iota // message_stop arrived
	outcomeRefused                // 429 at reserve
	outcomeCutOff                 // rate_limit_error event mid-stream
)

// replicaStats counts the outcomes that one replica gave.
type replicaStats struct {
	mu     sync.Mutex
	counts [3]int
}

func (s *replicaStats) add(o outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[o]++
}

// TestOverspendBound runs two gateway replicas, each with its own Postgres
// limiter and pool, on one database, and drives one team past its budget
// with concurrent long streams across both. It asserts the overspend bound
// as a number: the tokens settled in the window pass the budget by at most
// one top-up chunk per stream in flight. The client's worker count bounds
// the streams in flight. The default estimator counts more tokens than the
// mock reports, so the estimation-error term of the bound is not needed.
func TestOverspendBound(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	up := httptest.NewServer(mock.New(mock.Config{TokensPerSecond: 5000}))
	t.Cleanup(up.Close)
	if r := do(t, up.URL+"/_faults", `{"long_reply":true}`, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("set long_reply fault: status %d", r.StatusCode)
	}

	// A fixed clock keeps every reservation in one window and keeps leases
	// from ending, so the reaper charges nothing.
	at := time.Date(2001, 2, 3, 4, 5, 15, 0, time.UTC)
	clock := func() time.Time { return at }
	urls := make([]string, 2)
	for i := range urls {
		p := pool
		if i > 0 {
			if p, err = pgxpool.NewWithConfig(ctx, pool.Config()); err != nil {
				t.Fatalf("replica %d pool: %v", i, err)
			}
			t.Cleanup(p.Close)
		}
		l := postgres.New(p, postgres.WithClock(clock))
		if i == 0 {
			if err := l.SetBudget(ctx, "red", limit.Budget{Tokens: overspendBudget, Window: time.Hour}); err != nil {
				t.Fatalf("SetBudget: %v", err)
			}
		}
		bg, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); l.Run(bg) }()
		// Runs before the pool closes: cleanups run last in, first out.
		t.Cleanup(func() { stop(); <-done })

		h, err := New(Config{UpstreamURL: up.URL, UpstreamKey: upstreamKey, Authenticator: newAuth(t), Limiter: l})
		if err != nil {
			t.Fatal(err)
		}
		gs := httptest.NewServer(h)
		t.Cleanup(gs.Close)
		urls[i] = gs.URL + "/v1/messages"
	}

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: overspendWorkers}}
	t.Cleanup(client.CloseIdleConnections)
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stats := make([]replicaStats, len(urls))
	start := time.Now()
	var wg sync.WaitGroup
	for w := range overspendWorkers {
		wg.Go(func() {
			for n := range overspendPerWorker {
				i := (w + n) % len(urls)
				// A distinct prompt gives each request its own mock reply.
				body := fmt.Sprintf(`{"model":"claude-test","max_tokens":%d,"stream":true,"messages":[{"role":"user","content":"load %d %d"}]}`,
					overspendMaxTokens, w, n)
				o, err := overspendRequest(runCtx, client, urls[i], body)
				if err != nil {
					t.Errorf("worker %d request %d on replica %d: %v", w, n, i, err)
					return
				}
				stats[i].add(o)
				if o == outcomeRefused {
					return
				}
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	if t.Failed() {
		return
	}

	var settled, reserved, windows, inflight int64
	err = pool.QueryRow(ctx, `SELECT coalesce(sum(settled), 0), coalesce(sum(reserved), 0), count(*)
		FROM usage_windows WHERE team = 'red'`).Scan(&settled, &reserved, &windows)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT inflight FROM budgets WHERE team = 'red'`).Scan(&inflight); err != nil {
		t.Fatalf("read in-flight count: %v", err)
	}

	var requests int
	for i := range stats {
		c := stats[i].counts
		requests += c[outcomeOK] + c[outcomeRefused] + c[outcomeCutOff]
		t.Logf("replica %d: %d ok, %d cut off, %d refused", i, c[outcomeOK], c[outcomeCutOff], c[outcomeRefused])
		// The load must reach the limit on both replicas, or the bound
		// holds for free.
		if c[outcomeCutOff] == 0 || c[outcomeRefused] == 0 {
			t.Errorf("replica %d: %d cut off, %d refused; want at least one of each", i, c[outcomeCutOff], c[outcomeRefused])
		}
	}
	bound := int64(overspendBudget + overspendWorkers*topUpChunk)
	t.Logf("settled %d of budget %d: overspend %d tokens, bound %d (%d streams x %d); %d requests in %v (%.0f req/s)",
		settled, overspendBudget, settled-overspendBudget, bound-overspendBudget, overspendWorkers, topUpChunk,
		requests, elapsed.Round(time.Millisecond), float64(requests)/elapsed.Seconds())

	if windows != 1 {
		t.Errorf("usage windows = %d, want 1", windows)
	}
	if reserved != 0 || inflight != 0 {
		t.Errorf("after the run: reserved = %d, in flight = %d; want 0 and 0 (every stream settled)", reserved, inflight)
	}
	if settled > bound {
		t.Errorf("settled = %d, want <= %d: budget %d + %d streams x %d-token chunk",
			settled, bound, overspendBudget, overspendWorkers, topUpChunk)
	}
}

// overspendRequest sends one stream request and reads the reply to its
// end. The gateway settles before it ends the reply, so the window counts
// are final for this request when it returns.
func overspendRequest(ctx context.Context, c *http.Client, url, body string) (outcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = keyHeader()
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		_, err := io.Copy(io.Discard, resp.Body)
		return outcomeRefused, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	rd := anthropic.NewReader(resp.Body, anthropic.Limits{})
	var got outcome = -1
	for {
		e, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		switch e.Name {
		case anthropic.EventMessageStop:
			got = outcomeOK
		case anthropic.EventError:
			se, err := anthropic.ParseStreamEvent(e.Data)
			if err != nil || se.Error == nil || se.Error.Type != anthropic.ErrRateLimit {
				return 0, fmt.Errorf("unexpected error event: %s", e.Data)
			}
			got = outcomeCutOff
		}
	}
	if got < 0 {
		return 0, errors.New("stream ended without message_stop or an error event")
	}
	return got, nil
}
