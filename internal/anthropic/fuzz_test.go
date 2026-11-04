package anthropic

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// FuzzReader checks that the reader never panics, stays inside its limits,
// and that the raw bytes of the dispatched events are an exact prefix of the
// input.
func FuzzReader(f *testing.F) {
	f.Add([]byte(stream))
	f.Add([]byte("event: a\r\ndata: 1\r\n\r\n"))
	f.Add([]byte("data: a\rdata: b\r\r: c\n"))
	f.Add([]byte("\xEF\xBB\xBFdata\n\nevent: x\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		const maxLine, maxEvent = 256, 1024
		r := NewReader(bytes.NewReader(in), Limits{MaxLineBytes: maxLine, MaxEventBytes: maxEvent})
		var raw []byte
		for {
			ev, err := r.Next()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) &&
					!errors.Is(err, ErrLineTooLong) && !errors.Is(err, ErrEventTooLarge) {
					t.Fatalf("unexpected error: %v", err)
				}
				break
			}
			if len(ev.Raw) > maxEvent {
				t.Fatalf("event of %d bytes exceeds the limit", len(ev.Raw))
			}
			if len(ev.Data) > len(ev.Raw) {
				t.Fatalf("data longer than raw: %q", ev.Raw)
			}
			raw = append(raw, ev.Raw...)
		}
		if !bytes.HasPrefix(in, raw) {
			t.Fatalf("raw bytes are not a prefix of the input")
		}
	})
}

// FuzzRoundTrip checks that an event written by Writer reads back unchanged.
func FuzzRoundTrip(f *testing.F) {
	f.Add("message_start", []byte(`{"type":"message_start"}`))
	f.Add("", []byte("a\r\nb\rc\n"))
	f.Add(" e:x", []byte(""))
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		var b bytes.Buffer
		if err := NewWriter(&b).WriteEvent(name, data); err != nil {
			if !strings.ContainsAny(name, "\r\n") {
				t.Fatalf("WriteEvent(%q): %v", name, err)
			}
			return
		}
		ev, err := NewReader(&b, Limits{}).Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		wantName := name
		if wantName == "" {
			wantName = "message"
		}
		wantData := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		wantData = bytes.ReplaceAll(wantData, []byte("\r"), []byte("\n"))
		if ev.Name != wantName || !bytes.Equal(ev.Data, wantData) {
			t.Fatalf("got (%q, %q), want (%q, %q)", ev.Name, ev.Data, wantName, wantData)
		}
	})
}
