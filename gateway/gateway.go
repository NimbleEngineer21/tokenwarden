// Package gateway is an HTTP proxy for the Anthropic Messages API. It
// authenticates each client with a gateway key, replaces that key with the
// upstream key, and relays the reply. A stream reply is relayed one event at
// a time, with a flush after each event.
package gateway

import (
	"bytes"
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

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// maxRequestBytes is the request size limit of the Messages API.
const maxRequestBytes = 32 << 20

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
}

// gateway is the handler that [New] returns.
type gateway struct {
	upstream *url.URL
	key      string
	authn    auth.Authenticator
	client   *http.Client
	log      *slog.Logger
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
	reqID := requestID(w)
	key := clientKey(r)
	if key == "" {
		writeError(w, http.StatusUnauthorized, anthropic.ErrAuthentication, "x-api-key header is required", reqID)
		return
	}
	id, err := g.authn.Authenticate(ctx, key)
	if errors.Is(err, auth.ErrUnauthorized) {
		writeError(w, http.StatusUnauthorized, anthropic.ErrAuthentication, "invalid x-api-key", reqID)
		return
	}
	if err != nil {
		g.log.ErrorContext(ctx, "authenticator failed", "request_id", reqID, "err", err)
		writeError(w, http.StatusInternalServerError, anthropic.ErrAPI, "authentication is unavailable", reqID)
		return
	}
	log := g.log.With("request_id", reqID, "key_id", id.KeyID, "team", id.Team)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, anthropic.ErrRequestTooLarge, "request exceeds the size limit", reqID)
			return
		}
		writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, "could not read the request body", reqID)
		return
	}
	var p peek
	if err := json.Unmarshal(body, &p); err != nil {
		writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, "body is not valid JSON", reqID)
		return
	}
	log = log.With("model", p.Model, "stream", p.Stream)

	up := *g.upstream
	up.RawQuery = r.URL.RawQuery
	ureq, err := http.NewRequestWithContext(ctx, http.MethodPost, up.String(), bytes.NewReader(body))
	if err != nil {
		log.ErrorContext(ctx, "build upstream request", "err", err)
		writeError(w, http.StatusInternalServerError, anthropic.ErrAPI, "internal error", reqID)
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
			return
		}
		log.ErrorContext(ctx, "upstream request failed", "err", err)
		writeError(w, http.StatusBadGateway, anthropic.ErrAPI, "upstream is unavailable", reqID)
		return
	}
	defer resp.Body.Close()

	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized, s == http.StatusForbidden:
		// The upstream refused the gateway's own key. Passing the status on
		// would tell the client that its gateway key is wrong.
		log.ErrorContext(ctx, "upstream rejected the gateway credentials", "status", s)
		writeError(w, http.StatusBadGateway, anthropic.ErrAPI, "upstream rejected the gateway credentials", reqID)
		return
	case s < 200, s >= 300 && s < 400:
		log.ErrorContext(ctx, "unexpected upstream status", "status", s)
		writeError(w, http.StatusBadGateway, anthropic.ErrAPI, "unexpected upstream status", reqID)
		return
	}
	for _, h := range responseHeaders {
		if v := resp.Header.Values(h); len(v) > 0 {
			w.Header()[h] = v
		}
	}
	if resp.StatusCode < 300 && isEventStream(resp.Header.Get("Content-Type")) {
		relayStream(ctx, w, resp, log, reqID)
		return
	}
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "relay of upstream reply ended early", "err", err)
	}
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
