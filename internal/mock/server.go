package mock

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// maxRequestBytes is the request size limit of the Messages API.
const maxRequestBytes = 32 << 20

// Config sets the pace of the mock. The zero Config sends at full speed.
type Config struct {
	// TTFT is the time to first token: the wait before the response headers
	// and message_start.
	TTFT time.Duration
	// TokensPerSecond sets the wait before each content delta. Zero turns
	// the wait off.
	TokensPerSecond float64
	// Jitter varies each delta wait by up to this fraction, from 0 to 1.
	Jitter float64
	// Logger receives one line per request. Nil discards the logs. The mock
	// never logs API keys or prompt text.
	Logger *slog.Logger
}

// Server is the mock upstream. It serves POST /v1/messages and the fault
// API on /_faults.
type Server struct {
	cfg    Config
	log    *slog.Logger
	faults faults
	mux    *http.ServeMux
}

// New returns a Server with the pace in cfg.
func New(cfg Config) *Server {
	cfg.Jitter = min(max(cfg.Jitter, 0), 1)
	s := &Server{cfg: cfg, log: cfg.Logger, mux: http.NewServeMux()}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.mux.HandleFunc("POST /v1/messages", s.handleMessages)
	s.mux.HandleFunc("/_faults", s.handleFaults)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, anthropic.ErrNotFound, "not found", "")
	})
	return s
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := "req_mock_" + rand.Text()
	w.Header().Set("request-id", reqID)
	if r.Header.Get("x-api-key") == "" {
		writeError(w, http.StatusUnauthorized, anthropic.ErrAuthentication, "x-api-key header is required", reqID)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, anthropic.ErrRequestTooLarge, "request exceeds the size limit", reqID)
			return
		}
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, "body is not valid JSON", reqID)
		return
	}
	if msg := req.check(); msg != "" {
		writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, msg, reqID)
		return
	}
	sd := newSeed(body)
	rp, err := planReply(&req, sd, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, err.Error(), reqID)
		return
	}
	// The fault is taken only after the request is valid, so that a
	// rejected request does not use up a fault.
	f := s.faults.take()
	if f.LongReply {
		if rp, err = planReply(&req, sd, true); err != nil {
			writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, err.Error(), reqID)
			return
		}
	}
	log := s.log.With("request_id", reqID, "model", req.Model, "stream", req.Stream)
	if f != (Fault{}) {
		// Logged before the reply: a drop fault aborts the handler, so no
		// later line would show it.
		log.InfoContext(ctx, "fault applied", "fault", f)
	}
	if f.Status != 0 {
		if sleep(ctx, time.Duration(f.Delay)) != nil {
			return
		}
		if f.Status == http.StatusTooManyRequests {
			w.Header().Set("retry-after", "1")
		}
		writeError(w, f.Status, faultStatus[f.Status], "mock fault", reqID)
		return
	}
	p := &pacer{cfg: &s.cfg, r: sd.pace()}
	if req.Stream {
		err = s.stream(w, r, rp, p, f)
	} else {
		err = s.whole(w, r, rp, p, f)
	}
	if err != nil {
		log.InfoContext(ctx, "reply ended early", "err", err)
		return
	}
	log.InfoContext(ctx, "reply sent", "output_tokens", rp.outputTokens, "stop_reason", rp.stopReason)
}

// check returns the reason that req is invalid, or "".
func (r *request) check() string {
	switch {
	case r.Model == "":
		return "model: field required"
	case r.MaxTokens <= 0:
		return "max_tokens: must be at least 1"
	case len(r.Messages) == 0:
		return "messages: at least one message is required"
	}
	return ""
}

func writeError(w http.ResponseWriter, status int, errType, msg, reqID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(anthropic.NewErrorBody(errType, msg, reqID))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, anthropic.ErrAPI, "encode response", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
