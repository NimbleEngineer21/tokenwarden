package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// Outcomes that are not an HTTP status or a stream error type. See
// docs/loadgen.md for the full list.
const (
	outcomeOK        = "ok"
	outcomeTransport = "transport"
	outcomeTruncated = "stream_truncated"
	outcomeBadBody   = "bad_body"
	outcomeCanceled  = "canceled"
)

// maxBodyBytes bounds the non-stream reply that loadgen reads.
const maxBodyBytes = 32 << 20

// result is the record of one request.
type result struct {
	outcome string
	// ttft is the time to the first content delta. It is zero for a
	// non-stream request and for a stream without content.
	ttft    time.Duration
	latency time.Duration
	usage   anthropic.Usage
}

// generate sends requests from cfg.concurrency workers until the request
// count or the duration is used up, or ctx ends. A request that is in flight
// when the duration ends runs to completion.
func generate(ctx context.Context, cfg config, client *http.Client) []result {
	var (
		mu      sync.Mutex
		results []result
		count   atomic.Int64
		wg      sync.WaitGroup
	)
	start := time.Now()
	next := func() (int64, bool) {
		if ctx.Err() != nil || (cfg.duration > 0 && time.Since(start) >= cfg.duration) {
			return 0, false
		}
		n := count.Add(1)
		return n, cfg.requests == 0 || n <= int64(cfg.requests)
	}
	for range cfg.concurrency {
		wg.Go(func() {
			for {
				n, ok := next()
				if !ok {
					return
				}
				r := send(ctx, cfg, client, n)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return results
}

// send makes request number n and returns its record.
func send(ctx context.Context, cfg config, client *http.Client, n int64) result {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	start := time.Now()
	r := exchange(ctx, cfg, client, n, start)
	r.latency = time.Since(start)
	if r.outcome != outcomeOK && ctx.Err() != nil {
		r.outcome = outcomeCanceled
	}
	return r
}

func exchange(ctx context.Context, cfg config, client *http.Client, n int64, start time.Time) result {
	target := cfg.urls[int((n-1)%int64(len(cfg.urls)))] + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(requestBody(cfg, n)))
	if err != nil {
		return result{outcome: outcomeTransport}
	}
	req.Header.Set("x-api-key", cfg.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	res, err := client.Do(req) //nolint:gosec // G704: the operator names the gateway URL; it is not request input.
	if err != nil {
		return result{outcome: outcomeTransport}
	}
	body := res.Body
	defer body.Close()
	if res.StatusCode != http.StatusOK {
		// Drain the body so that the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBodyBytes))
		return result{outcome: "http_" + strconv.Itoa(res.StatusCode)}
	}
	if !cfg.stream {
		return readMessage(body)
	}
	return readStream(body, start)
}

// requestBody returns the Messages API request of request number n. The
// number makes each prompt differ, so that a seeded mock upstream does not
// send the same reply to every request.
func requestBody(cfg config, n int64) []byte {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	prompt := fmt.Sprintf("Request %d.", n) + strings.Repeat(" lorem", cfg.promptWords)
	b, err := json.Marshal(struct {
		Model     string    `json:"model"`
		MaxTokens int64     `json:"max_tokens"`
		Stream    bool      `json:"stream,omitempty"`
		Messages  []message `json:"messages"`
	}{cfg.model, cfg.maxTokens, cfg.stream, []message{{"user", prompt}}})
	if err != nil {
		panic(err) // The struct has only strings and numbers.
	}
	return b
}

func readMessage(body io.Reader) result {
	var m struct {
		Usage anthropic.Usage `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxBodyBytes)).Decode(&m); err != nil {
		return result{outcome: outcomeBadBody}
	}
	return result{outcome: outcomeOK, usage: m.Usage}
}

// readStream reads a stream reply until message_stop or an error event.
func readStream(body io.Reader, start time.Time) result {
	sr := anthropic.NewReader(body, anthropic.Limits{})
	var r result
	for {
		ev, err := sr.Next()
		if err != nil {
			// The stream ended before message_stop: a clean EOF or a cut
			// connection.
			r.outcome = outcomeTruncated
			return r
		}
		se, err := anthropic.ParseStreamEvent(ev.Data)
		if err != nil {
			r.outcome = outcomeBadBody
			return r
		}
		switch se.Type {
		case anthropic.EventMessageStart:
			if se.Message != nil {
				r.usage.Merge(se.Message.Usage)
			}
		case anthropic.EventContentBlockDelta:
			if r.ttft == 0 {
				r.ttft = time.Since(start)
			}
		case anthropic.EventMessageDelta:
			if se.Usage != nil {
				r.usage.Merge(*se.Usage)
			}
		case anthropic.EventError:
			errType := "unknown"
			if se.Error != nil && se.Error.Type != "" {
				errType = se.Error.Type
			}
			r.outcome = "stream_" + errType
			return r
		case anthropic.EventMessageStop:
			r.outcome = outcomeOK
			return r
		}
	}
}
