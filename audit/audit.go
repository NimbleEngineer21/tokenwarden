// Package audit records one event for each request that the gateway serves.
//
// An event holds who sent the request, what the gateway did with it, and the
// tokens that the upstream reported. It never holds a key, an Authorization
// value, or prompt or reply text.
package audit

import (
	"context"
	"log/slog"
	"time"
)

// Outcome is what the gateway did with a request.
type Outcome string

// Outcomes of a request.
const (
	// OutcomeOK means that the upstream reply reached the client in full.
	OutcomeOK Outcome = "ok"
	// OutcomeUnauthorized means that the gateway refused the gateway key.
	OutcomeUnauthorized Outcome = "unauthorized"
	// OutcomeInvalid means that the gateway refused the request before it
	// went upstream, for example because the body was not JSON.
	OutcomeInvalid Outcome = "invalid"
	// OutcomeUpstreamError means that the upstream answered with an error
	// status or an error stream event, and the gateway passed it on.
	OutcomeUpstreamError Outcome = "upstream_error"
	// OutcomeGatewayError means that the gateway could not get a usable
	// reply: the upstream was down, refused the gateway's own key, or sent
	// an unexpected status.
	OutcomeGatewayError Outcome = "gateway_error"
	// OutcomeIncomplete means that the upstream stream ended before
	// message_stop and the gateway sent an error event.
	OutcomeIncomplete Outcome = "incomplete"
	// OutcomeCanceled means that the client went away before the reply
	// ended.
	OutcomeCanceled Outcome = "canceled"
)

// Usage holds the token counts that the upstream reported.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// Event is the audit record of one request.
type Event struct {
	Time              time.Time
	RequestID         string
	UpstreamRequestID string
	// KeyID, Team, and User are empty when the gateway refused the key.
	KeyID string
	Team  string
	User  string
	Model string
	// Stream is the stream flag of the request.
	Stream bool
	// Status is the HTTP status sent to the client, or 0 when the client
	// went away before the gateway sent one.
	Status  int
	Outcome Outcome
	// ErrorType is the Messages API error type of a failed request, such as
	// "rate_limit_error".
	ErrorType string
	Usage     Usage
	Duration  time.Duration
}

// Recorder stores audit events. A Recorder must be safe for concurrent use.
type Recorder interface {
	// Record stores ev. The gateway logs a failure and goes on.
	Record(ctx context.Context, ev *Event) error
}

// Slog is a [Recorder] that writes each event as one log record.
type Slog struct {
	log *slog.Logger
}

// NewSlog returns a Recorder that writes to log at Info level.
func NewSlog(log *slog.Logger) *Slog {
	return &Slog{log: log}
}

// Record implements [Recorder].
func (s *Slog) Record(ctx context.Context, ev *Event) error {
	s.log.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.Time("time", ev.Time),
		slog.String("request_id", ev.RequestID),
		slog.String("upstream_request_id", ev.UpstreamRequestID),
		slog.String("key_id", ev.KeyID),
		slog.String("team", ev.Team),
		slog.String("user", ev.User),
		slog.String("model", ev.Model),
		slog.Bool("stream", ev.Stream),
		slog.Int("status", ev.Status),
		slog.String("outcome", string(ev.Outcome)),
		slog.String("error_type", ev.ErrorType),
		slog.Group("usage",
			slog.Int64("input_tokens", ev.Usage.InputTokens),
			slog.Int64("output_tokens", ev.Usage.OutputTokens),
			slog.Int64("cache_creation_input_tokens", ev.Usage.CacheCreationInputTokens),
			slog.Int64("cache_read_input_tokens", ev.Usage.CacheReadInputTokens),
		),
		slog.Duration("duration", ev.Duration),
	)
	return nil
}
