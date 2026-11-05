package anthropic

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type wantEvent struct {
	name, data string
}

func readAll(t *testing.T, in string, lim Limits) ([]Event, error) {
	t.Helper()
	r := NewReader(strings.NewReader(in), lim)
	var evs []Event
	for {
		ev, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return evs, nil
			}
			return evs, err
		}
		evs = append(evs, ev)
	}
}

func TestReader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []wantEvent
	}{
		{"lf", "event: ping\ndata: {}\n\n", []wantEvent{{"ping", "{}"}}},
		{"crlf", "event: ping\r\ndata: {}\r\n\r\n", []wantEvent{{"ping", "{}"}}},
		{"cr", "event: ping\rdata: {}\r\r", []wantEvent{{"ping", "{}"}}},
		{"default name", "data: x\n\n", []wantEvent{{"message", "x"}}},
		{"multi-line data", "data: a\ndata: b\ndata:\n\n", []wantEvent{{"message", "a\nb\n"}}},
		{"no space after colon", "event:e\ndata:x\n\n", []wantEvent{{"e", "x"}}},
		{"only one space stripped", "data:  x\n\n", []wantEvent{{"message", " x"}}},
		{"comments ignored", ": hi\nevent: e\n: there\ndata: x\n\n", []wantEvent{{"e", "x"}}},
		{"unknown, id, retry ignored", "id: 1\nretry: 5\nfoo: bar\ndata: x\n\n", []wantEvent{{"message", "x"}}},
		{"field without colon", "data\n\n", []wantEvent{{"message", ""}}},
		{"event without data skipped", "event: a\n\nevent: b\ndata: x\n\n", []wantEvent{{"b", "x"}}},
		{"bom stripped", "\xEF\xBB\xBFevent: e\ndata: x\n\n", []wantEvent{{"e", "x"}}},
		{"trailing comment at eof", "data: x\n\n: bye", []wantEvent{{"message", "x"}}},
		{"two events", "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n", []wantEvent{{"a", "1"}, {"b", "2"}}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, err := readAll(t, tt.in, Limits{})
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if len(evs) != len(tt.want) {
				t.Fatalf("got %d events, want %d", len(evs), len(tt.want))
			}
			var raw []byte
			for i, ev := range evs {
				if ev.Name != tt.want[i].name || string(ev.Data) != tt.want[i].data {
					t.Errorf("event %d = (%q, %q), want (%q, %q)", i, ev.Name, ev.Data, tt.want[i].name, tt.want[i].data)
				}
				raw = append(raw, ev.Raw...)
			}
			if !strings.HasPrefix(tt.in, string(raw)) {
				t.Errorf("raw bytes %q are not a prefix of the input", raw)
			}
		})
	}
}

func TestReaderErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		lim  Limits
		want error
	}{
		{"truncated after field", "event: a\ndata: x\n", Limits{}, io.ErrUnexpectedEOF},
		{"truncated mid-line", "data: x", Limits{}, io.ErrUnexpectedEOF},
		{"line too long", "data: " + strings.Repeat("x", 64) + "\n\n", Limits{MaxLineBytes: 32}, ErrLineTooLong},
		{"event too large", strings.Repeat("data: x\n", 10) + "\n", Limits{MaxEventBytes: 40}, ErrEventTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readAll(t, tt.in, tt.lim)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// oneByteReader returns one byte per Read, so a CRLF pair is split across
// reads.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestReaderSplitCRLF(t *testing.T) {
	r := NewReader(oneByteReader{strings.NewReader("data: a\r\n\r\ndata: b\r\n\r\n")}, Limits{})
	for _, want := range []string{"a", "b"} {
		ev, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if string(ev.Data) != want {
			t.Errorf("data = %q, want %q", ev.Data, want)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestWriteEvent(t *testing.T) {
	tests := []struct {
		name, ev, data, want string
	}{
		{"simple", "ping", `{"type":"ping"}`, "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{"no name", "", "x", "data: x\n\n"},
		{"empty data", "e", "", "event: e\ndata: \n\n"},
		{"line breaks split", "e", "a\nb\r\nc\rd", "event: e\ndata: a\ndata: b\ndata: c\ndata: d\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := NewWriter(&b).WriteEvent(tt.ev, []byte(tt.data)); err != nil {
				t.Fatalf("WriteEvent: %v", err)
			}
			if b.String() != tt.want {
				t.Errorf("got %q, want %q", b.String(), tt.want)
			}
		})
	}
}

func TestWriteEventRejectsBadName(t *testing.T) {
	var b bytes.Buffer
	if err := NewWriter(&b).WriteEvent("a\nb", nil); err == nil {
		t.Fatal("WriteEvent accepted a name with a line break")
	}
	if b.Len() != 0 {
		t.Errorf("wrote %q after an error", b.String())
	}
}

func TestWriterFlushesEachEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewWriter(rec)
	if err := w.WriteEvent("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed {
		t.Fatal("event was not flushed")
	}
	rec.Flushed = false
	if err := w.WriteRaw([]byte("data: 2\n\n")); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed {
		t.Fatal("raw event was not flushed")
	}
}
