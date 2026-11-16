package gateway

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
	"github.com/NimbleEngineer21/tokenwarden/limit"
)

// errIncomplete is the message of the error event that the gateway sends
// when the upstream stream ends without message_stop.
const errIncomplete = "upstream stream ended before message_stop"

func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/event-stream"
}

// relayStream copies an SSE reply to the client one event at a time and
// flushes after each event. A stream that ends without message_stop or an
// upstream error event gets an error event of its own, so that the client
// never takes a cut-off reply for a whole one. It reads the usage from
// message_start and message_delta into ev. With a budget b, it tops up the
// reservation before it sends generated text past the held amount, and it
// cuts the stream off with an error event when the top-up fails.
func relayStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, log *slog.Logger, ev *audit.Event, b *budget, m *metrics) {
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	// Tells a buffering reverse proxy in front of the gateway, such as
	// nginx, to pass each event on at once.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	sw := anthropic.NewWriter(w)
	rd := anthropic.NewReader(resp.Body, anthropic.Limits{})
	var usage anthropic.Usage
	defer func() { ev.Usage = auditUsage(usage) }()
	var events int
	for {
		e, err := rd.Next()
		switch {
		case err == nil:
		case ev.Outcome != "":
			// message_stop or an upstream error event was relayed; what
			// follows does not change the reply.
			return
		case ctx.Err() != nil:
			ev.Outcome = audit.OutcomeCanceled
			return
		default:
			log.WarnContext(ctx, errIncomplete, "err", err, "events", events)
			ev.Outcome, ev.ErrorType = audit.OutcomeIncomplete, anthropic.ErrAPI
			_ = sw.WriteError(anthropic.ErrAPI, errIncomplete, ev.RequestID)
			return
		}
		if b != nil && e.Name == anthropic.EventContentBlockDelta {
			if err := b.ensure(ctx, m, deltaBytes(e.Data)); err != nil {
				cutOff(ctx, sw, log, ev, m, err)
				return
			}
		}
		events++
		if err := sw.WriteRaw(e.Raw); err != nil {
			// The client is gone; returning closes the upstream body.
			ev.Outcome = audit.OutcomeCanceled
			return
		}
		readEvent(e, &usage, ev)
	}
}

// readEvent takes the usage and the end of the stream from one event. Only
// the event types that carry them are decoded: a delta is relayed without a
// JSON parse.
func readEvent(e anthropic.Event, usage *anthropic.Usage, ev *audit.Event) {
	switch e.Name {
	case anthropic.EventMessageStart, anthropic.EventMessageDelta, anthropic.EventError:
	case anthropic.EventMessageStop:
		if ev.Outcome == "" {
			ev.Outcome = audit.OutcomeOK
		}
		return
	default:
		return
	}
	se, err := anthropic.ParseStreamEvent(e.Data)
	if err != nil {
		return
	}
	switch {
	case se.Message != nil:
		usage.Merge(se.Message.Usage)
	case se.Usage != nil:
		usage.Merge(*se.Usage)
	}
	if e.Name == anthropic.EventError {
		ev.Outcome = audit.OutcomeUpstreamError
		if se.Error != nil {
			ev.ErrorType = se.Error.Type
		}
	}
}

// deltaBytes returns the bytes of generated text in a content_block_delta.
// A delta that does not parse counts as its whole data, so that it never
// passes the budget for free.
func deltaBytes(data []byte) int64 {
	se, err := anthropic.ParseStreamEvent(data)
	if err != nil {
		return int64(len(data))
	}
	if se.Delta == nil {
		return 0
	}
	return int64(len(se.Delta.Text) + len(se.Delta.PartialJSON) + len(se.Delta.Thinking))
}

// cutOff ends a stream whose top-up failed with an error event in place of
// the delta that did not fit.
func cutOff(ctx context.Context, sw *anthropic.Writer, log *slog.Logger, ev *audit.Event, m *metrics, err error) {
	errType, msg := anthropic.ErrRateLimit, errMsgExceeded
	switch {
	case errors.Is(err, limit.ErrBudgetExceeded):
		m.denials.WithLabelValues(ev.Team, "topup").Inc()
		ev.Outcome = audit.OutcomeBudgetExceeded
	case ctx.Err() != nil:
		ev.Outcome = audit.OutcomeCanceled
		return
	default:
		log.ErrorContext(ctx, "top-up failed", "err", err)
		errType, msg = anthropic.ErrAPI, errMsgUnavailable
		ev.Outcome = audit.OutcomeGatewayError
	}
	ev.ErrorType = errType
	_ = sw.WriteError(errType, msg, ev.RequestID)
}
