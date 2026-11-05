package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// pacer computes the wait before each content delta.
type pacer struct {
	cfg *Config
	r   *rand.Rand
}

func (p *pacer) delta(tokens int) time.Duration {
	if p.cfg.TokensPerSecond <= 0 {
		return 0
	}
	d := float64(time.Second) * float64(tokens) / p.cfg.TokensPerSecond
	if p.cfg.Jitter > 0 {
		d *= 1 + p.cfg.Jitter*(2*p.r.Float64()-1)
	}
	return time.Duration(d)
}

// sleep waits for d or until ctx ends, so that a client disconnect stops a
// paced reply at once.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// emitter sends stream events and applies a drop fault.
type emitter struct {
	w         *anthropic.Writer
	sent      int
	dropAfter *int
	midEvent  bool
}

func (e *emitter) emit(ev *wireEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if e.dropAfter != nil && e.sent == *e.dropAfter {
		if e.midEvent {
			raw := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", ev.Type, data)
			_ = e.w.WriteRaw(raw[:len(raw)/2])
		}
		// ErrAbortHandler makes net/http close the connection without a
		// clean end of the chunked body, which is what a dropped upstream
		// looks like to the client.
		panic(http.ErrAbortHandler)
	}
	e.sent++
	return e.w.WriteEvent(ev.Type, data)
}

// stream sends rp as a paced SSE stream, one flush per event.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, rp *reply, p *pacer, f Fault) error {
	ctx := r.Context()
	if err := sleep(ctx, time.Duration(f.Delay)+s.cfg.TTFT); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	e := &emitter{w: anthropic.NewWriter(w), dropAfter: f.DropAfterEvents, midEvent: f.DropMidEvent}

	start := rp.message(false)
	start.Usage.OutputTokens = 1
	if err := e.emit(&wireEvent{Type: anthropic.EventMessageStart, Message: start}); err != nil {
		return err
	}
	for i, b := range rp.blocks {
		hdr := wireBlock{Type: b.kind}
		if b.kind == anthropic.BlockToolUse {
			hdr.ID, hdr.Name, hdr.Input = b.id, b.name, json.RawMessage("{}")
		} else {
			hdr.Text = new(string)
		}
		if err := e.emit(&wireEvent{Type: anthropic.EventContentBlockStart, Index: &i, ContentBlock: &hdr}); err != nil {
			return err
		}
		if i == 0 {
			if err := e.emit(&wireEvent{Type: anthropic.EventPing}); err != nil {
				return err
			}
		}
		for _, c := range b.chunks {
			if err := sleep(ctx, p.delta(c.tokens)); err != nil {
				return err
			}
			var d any = textDelta{Type: anthropic.DeltaText, Text: c.text}
			if b.kind == anthropic.BlockToolUse {
				d = jsonDelta{Type: anthropic.DeltaInputJSON, PartialJSON: c.text}
			}
			if err := e.emit(&wireEvent{Type: anthropic.EventContentBlockDelta, Index: &i, Delta: d}); err != nil {
				return err
			}
		}
		if err := e.emit(&wireEvent{Type: anthropic.EventContentBlockStop, Index: &i}); err != nil {
			return err
		}
	}
	usage := anthropic.Usage{InputTokens: rp.inputTokens, OutputTokens: rp.outputTokens}
	if err := e.emit(&wireEvent{
		Type:  anthropic.EventMessageDelta,
		Delta: messageDelta{StopReason: rp.stopReason},
		Usage: &usage,
	}); err != nil {
		return err
	}
	return e.emit(&wireEvent{Type: anthropic.EventMessageStop})
}

// whole sends rp as one JSON message after the time that the stream would
// take.
func (s *Server) whole(w http.ResponseWriter, r *http.Request, rp *reply, p *pacer, f Fault) error {
	total := time.Duration(f.Delay) + s.cfg.TTFT
	for _, b := range rp.blocks {
		for _, c := range b.chunks {
			total += p.delta(c.tokens)
		}
	}
	if err := sleep(r.Context(), total); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rp.message(true))
	return nil
}

// message returns the message object. With content false it is the
// message_start form: empty content and no stop reason.
func (rp *reply) message(content bool) *wireMessage {
	m := &wireMessage{
		ID:      rp.id,
		Type:    "message",
		Role:    "assistant",
		Model:   rp.model,
		Content: []wireBlock{},
		Usage:   anthropic.Usage{InputTokens: rp.inputTokens},
	}
	if !content {
		return m
	}
	for _, b := range rp.blocks {
		wb := wireBlock{Type: b.kind}
		if b.kind == anthropic.BlockToolUse {
			wb.ID, wb.Name, wb.Input = b.id, b.name, b.input
		} else {
			t := b.text()
			wb.Text = &t
		}
		m.Content = append(m.Content, wb)
	}
	m.StopReason = &rp.stopReason
	m.Usage.OutputTokens = rp.outputTokens
	return m
}
