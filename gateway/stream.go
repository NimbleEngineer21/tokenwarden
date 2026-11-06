package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
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
// never takes a cut-off reply for a whole one.
func relayStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, log *slog.Logger, reqID string) {
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	// Tells a buffering reverse proxy in front of the gateway, such as
	// nginx, to pass each event on at once.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	sw := anthropic.NewWriter(w)
	rd := anthropic.NewReader(resp.Body, anthropic.Limits{})
	var events int
	var final bool // message_stop or an upstream error event was relayed
	for {
		ev, err := rd.Next()
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return
		case errors.Is(err, io.EOF) && final:
			return
		default:
			log.WarnContext(ctx, errIncomplete, "err", err, "events", events)
			_ = sw.WriteError(anthropic.ErrAPI, errIncomplete, reqID)
			return
		}
		events++
		if err := sw.WriteRaw(ev.Raw); err != nil {
			// The client is gone; returning closes the upstream body.
			return
		}
		final = final || ev.Name == anthropic.EventMessageStop || ev.Name == anthropic.EventError
	}
}
