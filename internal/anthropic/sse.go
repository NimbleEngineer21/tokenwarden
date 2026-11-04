package anthropic

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Default limits for a [Reader]. Large single events occur in practice (for
// example, server tool results in content_block_start), so the limits are
// generous; they exist to bound memory per stream, not to shape traffic.
const (
	DefaultMaxLineBytes  = 4 << 20
	DefaultMaxEventBytes = 8 << 20
)

// Errors returned by [Reader.Next].
var (
	// ErrLineTooLong means one SSE line exceeded the reader's line limit.
	ErrLineTooLong = errors.New("anthropic: sse line too long")
	// ErrEventTooLarge means one SSE event exceeded the reader's event limit.
	ErrEventTooLarge = errors.New("anthropic: sse event too large")
)

// Event is one dispatched server-sent event.
type Event struct {
	// Name is the event type from the "event" field, or "message" when the
	// field is absent, as the SSE specification requires.
	Name string
	// Data is the joined "data" field values, without the final newline.
	Data []byte
	// Raw is every byte consumed from the stream for this event, including
	// comments and the terminating blank line. Writing Raw reproduces the
	// upstream bytes exactly.
	Raw []byte
}

// Limits bounds the memory that a [Reader] uses. A zero field takes its
// default value.
type Limits struct {
	MaxLineBytes  int
	MaxEventBytes int
}

// Reader parses an SSE stream as defined by the WHATWG HTML specification,
// section "Server-sent events".
type Reader struct {
	sc       *bufio.Scanner
	maxEvent int
	first    bool
}

// NewReader returns a Reader that reads events from r.
func NewReader(r io.Reader, lim Limits) *Reader {
	if lim.MaxLineBytes <= 0 {
		lim.MaxLineBytes = DefaultMaxLineBytes
	}
	if lim.MaxEventBytes <= 0 {
		lim.MaxEventBytes = DefaultMaxEventBytes
	}
	sc := bufio.NewScanner(r)
	// The token includes the line terminator, so allow two extra bytes.
	sc.Buffer(make([]byte, 0, min(lim.MaxLineBytes+2, 64<<10)), lim.MaxLineBytes+2)
	sc.Split(splitLine)
	return &Reader{sc: sc, maxEvent: lim.MaxEventBytes, first: true}
}

// Next returns the next event. It returns io.EOF after the last complete
// event, and io.ErrUnexpectedEOF when the stream ends inside an event: for a
// proxy, a truncated event means the upstream connection dropped.
func (r *Reader) Next() (Event, error) {
	var (
		raw     []byte
		data    []byte
		name    string
		hasData bool
		pending bool // a field line was seen since the last dispatch
	)
	for r.sc.Scan() {
		tok := r.sc.Bytes()
		if len(raw)+len(tok) > r.maxEvent {
			return Event{}, ErrEventTooLarge
		}
		raw = append(raw, tok...)
		line := trimEOL(tok)
		if r.first {
			r.first = false
			line = bytes.TrimPrefix(line, utf8BOM)
		}
		if len(line) == 0 {
			if hasData {
				if name == "" {
					name = "message"
				}
				return Event{Name: name, Data: bytes.TrimSuffix(data, []byte("\n")), Raw: raw}, nil
			}
			// An event without data is not dispatched. Its bytes stay in raw
			// so that the next event forwards them unchanged.
			name, pending = "", false
			continue
		}
		if line[0] == ':' {
			continue
		}
		pending = true
		field, value, found := bytes.Cut(line, []byte(":"))
		if found {
			value = bytes.TrimPrefix(value, []byte(" "))
		}
		switch string(field) {
		case "event":
			name = string(value)
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
			hasData = true
		}
		// "id", "retry", and unknown fields do not affect the gateway.
	}
	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, ErrLineTooLong
		}
		return Event{}, err
	}
	if pending {
		return Event{}, io.ErrUnexpectedEOF
	}
	return Event{}, io.EOF
}

// splitLine is a bufio.SplitFunc for SSE lines, which end in CRLF, LF, or a
// lone CR. The token keeps its terminator so that Event.Raw is exact.
func splitLine(buf []byte, atEOF bool) (int, []byte, error) {
	i := bytes.IndexAny(buf, "\r\n")
	switch {
	case i < 0:
		if atEOF && len(buf) > 0 {
			return len(buf), buf, nil
		}
		return 0, nil, nil
	case buf[i] == '\n':
		return i + 1, buf[:i+1], nil
	case i+1 < len(buf):
		if buf[i+1] == '\n' {
			return i + 2, buf[:i+2], nil
		}
		return i + 1, buf[:i+1], nil
	case atEOF:
		return i + 1, buf[:i+1], nil
	default:
		// A CR at the end of the buffer: read more to see if LF follows.
		return 0, nil, nil
	}
}

var utf8BOM = []byte("\xEF\xBB\xBF")

func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

// Writer writes SSE events and flushes after each one, so that a client sees
// every event when it is sent rather than when a buffer fills.
type Writer struct {
	w     io.Writer
	flush func() error
}

// NewWriter returns a Writer to w. When w is an [http.ResponseWriter], the
// Writer flushes through [http.ResponseController]; otherwise it calls a
// Flush method on w if one exists.
func NewWriter(w io.Writer) *Writer {
	sw := &Writer{w: w, flush: func() error { return nil }}
	switch f := w.(type) {
	case http.ResponseWriter:
		rc := http.NewResponseController(f)
		sw.flush = rc.Flush
	case interface{ Flush() error }:
		sw.flush = f.Flush
	}
	return sw
}

// WriteRaw writes bytes that are already SSE-encoded, normally [Event.Raw],
// and flushes.
func (w *Writer) WriteRaw(raw []byte) error {
	if _, err := w.w.Write(raw); err != nil {
		return err
	}
	return w.flush()
}

// WriteEvent encodes one event and flushes. Each line of data becomes one
// "data" field. An empty name omits the "event" field.
func (w *Writer) WriteEvent(name string, data []byte) error {
	if strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("anthropic: sse event name %q contains a line break", name)
	}
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	for {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			break
		}
		writeData(&b, data[:i])
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			i++
		}
		data = data[i+1:]
	}
	writeData(&b, data)
	b.WriteByte('\n')
	return w.WriteRaw(b.Bytes())
}

func writeData(b *bytes.Buffer, line []byte) {
	b.WriteString("data: ")
	b.Write(line)
	b.WriteByte('\n')
}
