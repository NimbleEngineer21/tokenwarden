// Package gateway is an HTTP proxy for the Anthropic Messages API. It
// authenticates each client with a gateway key, replaces that key with the
// upstream key, and relays the reply. A stream reply is relayed one event at
// a time, with a flush after each event.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// maxRequestBytes is the request size limit of the Messages API.
const maxRequestBytes = 32 << 20

// maxCaptureBytes bounds the copy of a non-stream reply that the gateway
// keeps to read the usage or the error type. A larger reply is still relayed
// in full; only its usage goes unrecorded.
const maxCaptureBytes = 8 << 20

// RequestIDHeader is the response header that carries the gateway's own
// request ID. The upstream request ID, when there is one, stays in
// "request-id".
const RequestIDHeader = "X-Tokenwarden-Request-Id"

// Config configures a gateway. UpstreamURL, UpstreamKey, and Authenticator
// are required.
type Config struct {
	// UpstreamURL is the base URL of the Messages API, for example
	// "https://api.anthropic.com". The gateway appends "/v1/messages".
	UpstreamURL string
	// UpstreamKey is the API key that the gateway sends upstream in place
	// of the client's gateway key.
	UpstreamKey string
	// Authenticator maps a gateway key to its identity.
	Authenticator auth.Authenticator
	// Client sends the upstream requests. Nil uses a client with a clone of
	// [http.DefaultTransport]. The gateway never follows redirects, whatever
	// the client's CheckRedirect says.
	Client *http.Client
	// Logger receives operational logs. Nil discards them. The gateway never
	// logs keys or prompt bodies.
	Logger *slog.Logger
	// Recorder receives one audit event per request to /v1/messages. Nil
	// records nothing.
	Recorder audit.Recorder
	// Registerer receives the gateway's Prometheus collectors. Nil keeps the
	// metrics unregistered.
	Registerer prometheus.Registerer
}

// gateway is the handler that [New] returns.
type gateway struct {
	upstream *url.URL
	key      string
	authn    auth.Authenticator
	client   *http.Client
	log      *slog.Logger
	rec      audit.Recorder
	metrics  *metrics
	mux      *http.ServeMux
}

// New returns the gateway handler. It returns an error when cfg lacks a
// required field, so that a gateway with no upstream key fails at start and
// never answers in place of the upstream.
func New(cfg Config) (http.Handler, error) {
	u, err := url.Parse(cfg.UpstreamURL)
	switch {
	case cfg.UpstreamURL == "":
		return nil, errors.New("gateway: upstream URL is required")
	case err != nil:
		return nil, fmt.Errorf("gateway: upstream URL: %w", err)
	case u.Scheme != "http" && u.Scheme != "https", u.Host == "":
		return nil, errors.New("gateway: upstream URL must be an absolute http or https URL")
	case u.User != nil, u.RawQuery != "", u.Fragment != "":
		return nil, errors.New("gateway: upstream URL must not have user info, a query, or a fragment")
	case cfg.UpstreamKey == "":
		return nil, errors.New("gateway: upstream key is required")
	case strings.ContainsAny(cfg.UpstreamKey, " \t\r\n"):
		return nil, errors.New("gateway: upstream key contains white space")
	case cfg.Authenticator == nil:
		return nil, errors.New("gateway: authenticator is required")
	}
	m, err := newMetrics(cfg.Registerer)
	if err != nil {
		return nil, fmt.Errorf("gateway: register metrics: %w", err)
	}
	c := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	if cfg.Client != nil {
		cp := *cfg.Client
		c = &cp
	}
	// A redirect would carry the upstream key to the new location: Go strips
	// Authorization on a cross-host redirect, but not x-api-key.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	g := &gateway{
		upstream: u.JoinPath("v1", "messages"),
		key:      cfg.UpstreamKey,
		authn:    cfg.Authenticator,
		client:   c,
		log:      cfg.Logger,
		rec:      cfg.Recorder,
		metrics:  m,
		mux:      http.NewServeMux(),
	}
	if g.log == nil {
		g.log = slog.New(slog.DiscardHandler)
	}
	g.mux.HandleFunc("POST /v1/messages", g.handleMessages)
	g.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, anthropic.ErrNotFound, "not found", requestID(w))
	})
	return g, nil
}

// ServeHTTP implements [http.Handler].
func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(RequestIDHeader, "req_tw_"+rand.Text())
	g.mux.ServeHTTP(w, r)
}

// requestHeaders are the client headers that the gateway sends upstream.
// Every other header, the client's keys and cookies among them, stays at the
// gateway.
var requestHeaders = []string{"Anthropic-Version", "Anthropic-Beta", "Content-Type", "Accept"}

// responseHeaders are the upstream headers that the gateway returns. The
// anthropic-ratelimit-* headers are left out: they show the limits of the
// upstream account, not of the client.
var responseHeaders = []string{"Request-Id", "Retry-After", "Content-Type"}

// peek is the part of the request body that the gateway reads.
type peek struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (g *gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ev := &audit.Event{Time: time.Now(), RequestID: requestID(w)}
	defer g.finish(ctx, ev)
	key := clientKey(r)
	if key == "" {
		fail(w, ev, audit.OutcomeUnauthorized, http.StatusUnauthorized, anthropic.ErrAuthentication, "x-api-key header is required")
		return
	}
	id, err := g.authn.Authenticate(ctx, key)
	if errors.Is(err, auth.ErrUnauthorized) {
		fail(w, ev, audit.OutcomeUnauthorized, http.StatusUnauthorized, anthropic.ErrAuthentication, "invalid x-api-key")
		return
	}
	if err != nil {
		g.log.ErrorContext(ctx, "authenticator failed", "request_id", ev.RequestID, "err", err)
		fail(w, ev, audit.OutcomeGatewayError, http.StatusInternalServerError, anthropic.ErrAPI, "authentication is unavailable")
		return
	}
	ev.KeyID, ev.Team, ev.User = id.KeyID, id.Team, id.User
	log := g.log.With("request_id", ev.RequestID, "key_id", id.KeyID, "team", id.Team)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			fail(w, ev, audit.OutcomeInvalid, http.StatusRequestEntityTooLarge, anthropic.ErrRequestTooLarge, "request exceeds the size limit")
			return
		}
		fail(w, ev, audit.OutcomeInvalid, http.StatusBadRequest, anthropic.ErrInvalidRequest, "could not read the request body")
		return
	}
	var p peek
	if err := json.Unmarshal(body, &p); err != nil {
		fail(w, ev, audit.OutcomeInvalid, http.StatusBadRequest, anthropic.ErrInvalidRequest, "body is not valid JSON")
		return
	}
	ev.Model, ev.Stream = p.Model, p.Stream
	log = log.With("model", p.Model, "stream", p.Stream)

	up := *g.upstream
	up.RawQuery = r.URL.RawQuery
	ureq, err := http.NewRequestWithContext(ctx, http.MethodPost, up.String(), bytes.NewReader(body))
	if err != nil {
		log.ErrorContext(ctx, "build upstream request", "err", err)
		fail(w, ev, audit.OutcomeGatewayError, http.StatusInternalServerError, anthropic.ErrAPI, "internal error")
		return
	}
	for _, h := range requestHeaders {
		if v := r.Header.Values(h); len(v) > 0 {
			ureq.Header[h] = v
		}
	}
	if ureq.Header.Get("Content-Type") == "" {
		ureq.Header.Set("Content-Type", "application/json")
	}
	ureq.Header.Set("X-Api-Key", g.key)

	resp, err := g.client.Do(ureq)
	if err != nil {
		if ctx.Err() != nil {
			ev.Outcome = audit.OutcomeCanceled
			return
		}
		log.ErrorContext(ctx, "upstream request failed", "err", err)
		fail(w, ev, audit.OutcomeGatewayError, http.StatusBadGateway, anthropic.ErrAPI, "upstream is unavailable")
		return
	}
	defer resp.Body.Close()
	ev.UpstreamRequestID = resp.Header.Get("Request-Id")

	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized, s == http.StatusForbidden:
		// The upstream refused the gateway's own key. Passing the status on
		// would tell the client that its gateway key is wrong.
		log.ErrorContext(ctx, "upstream rejected the gateway credentials", "status", s)
		fail(w, ev, audit.OutcomeGatewayError, http.StatusBadGateway, anthropic.ErrAPI, "upstream rejected the gateway credentials")
		return
	case s < 200, s >= 300 && s < 400:
		log.ErrorContext(ctx, "unexpected upstream status", "status", s)
		fail(w, ev, audit.OutcomeGatewayError, http.StatusBadGateway, anthropic.ErrAPI, "unexpected upstream status")
		return
	}
	for _, h := range responseHeaders {
		if v := resp.Header.Values(h); len(v) > 0 {
			w.Header()[h] = v
		}
	}
	ev.Status = resp.StatusCode
	if resp.StatusCode < 300 && isEventStream(resp.Header.Get("Content-Type")) {
		g.metrics.active.Inc()
		defer g.metrics.active.Dec()
		relayStream(ctx, w, resp, log, ev)
		return
	}
	relayWhole(ctx, w, resp, log, ev)
}

// relayWhole copies a non-stream reply and reads its usage or error type
// from a bounded copy of the body.
func relayWhole(ctx context.Context, w http.ResponseWriter, resp *http.Response, log *slog.Logger, ev *audit.Event) {
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	keep := &capture{limit: maxCaptureBytes}
	if _, err := io.Copy(w, io.TeeReader(resp.Body, keep)); err != nil {
		if ctx.Err() != nil {
			ev.Outcome = audit.OutcomeCanceled
			return
		}
		log.WarnContext(ctx, "relay of upstream reply ended early", "err", err)
		ev.Outcome = audit.OutcomeIncomplete
		return
	}
	if resp.StatusCode >= 300 {
		ev.Outcome = audit.OutcomeUpstreamError
		var e anthropic.ErrorBody
		if json.Unmarshal(keep.buf.Bytes(), &e) == nil {
			ev.ErrorType = e.Error.Type
		}
		return
	}
	ev.Outcome = audit.OutcomeOK
	var m struct {
		Usage anthropic.Usage `json:"usage"`
	}
	if keep.over {
		log.WarnContext(ctx, "reply too large to read its usage", "limit", maxCaptureBytes)
	} else if err := json.Unmarshal(keep.buf.Bytes(), &m); err != nil {
		log.WarnContext(ctx, "could not read the usage of the reply", "err", err)
	}
	ev.Usage = auditUsage(m.Usage)
}

// capture keeps the first bytes written to it, up to limit. Past the limit it
// drops what it kept and sets over, and it never returns an error, so that
// the relay that tees into it never stops.
type capture struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capture) Write(p []byte) (int, error) {
	if !c.over {
		if c.buf.Len()+len(p) > c.limit {
			c.over = true
			c.buf = bytes.Buffer{}
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func auditUsage(u anthropic.Usage) audit.Usage {
	return audit.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
}

// finish records the audit event and the metrics of a request.
func (g *gateway) finish(ctx context.Context, ev *audit.Event) {
	ev.Duration = time.Since(ev.Time)
	g.metrics.observe(ev)
	if g.rec == nil {
		return
	}
	// The record must outlive a client that has gone away.
	if err := g.rec.Record(context.WithoutCancel(ctx), ev); err != nil {
		g.log.ErrorContext(ctx, "audit record failed", "request_id", ev.RequestID, "err", err)
	}
}

// fail sends a gateway error reply and notes it in ev.
func fail(w http.ResponseWriter, ev *audit.Event, outcome audit.Outcome, status int, errType, msg string) {
	ev.Outcome, ev.Status, ev.ErrorType = outcome, status, errType
	writeError(w, status, errType, msg, ev.RequestID)
}

// clientKey returns the gateway key from x-api-key, or from a Bearer token
// in Authorization when x-api-key is absent.
func clientKey(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	return ""
}

func requestID(w http.ResponseWriter) string {
	return w.Header().Get(RequestIDHeader)
}

func writeError(w http.ResponseWriter, status int, errType, msg, reqID string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(anthropic.NewErrorBody(errType, msg, reqID))
}
