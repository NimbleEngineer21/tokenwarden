package mock

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// maxFaultDelay bounds a delay fault, so that a typo cannot hang a demo.
const maxFaultDelay = 10 * time.Minute

// Duration is a time.Duration that encodes to JSON as a string such as "2s".
type Duration time.Duration

// MarshalJSON encodes d as a duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a duration string such as "1.5s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Fault is the misbehavior that the mock applies to the next requests to
// /v1/messages. The zero Fault changes nothing.
type Fault struct {
	// Delay is an extra wait before the response headers.
	Delay Duration `json:"delay,omitempty"`
	// Status, when set, makes the mock answer with this error status after
	// Delay: 429 (rate_limit_error), 500 (api_error), or 529
	// (overloaded_error).
	Status int `json:"status,omitempty"`
	// DropAfterEvents, when set, makes the mock close the connection of a
	// stream after this many events, so that no message_stop arrives.
	DropAfterEvents *int `json:"drop_after_events,omitempty"`
	// DropMidEvent makes the drop send half of the next event first, so that
	// the stream ends inside an event.
	DropMidEvent bool `json:"drop_mid_event,omitempty"`
	// LongReply makes the reply text fill max_tokens.
	LongReply bool `json:"long_reply,omitempty"`
	// Count is the number of requests that the fault applies to. Zero means
	// every request until the fault is cleared. In a GET response it is the
	// number of requests left.
	Count int `json:"count,omitempty"`
}

var faultStatus = map[int]string{
	http.StatusTooManyRequests:     anthropic.ErrRateLimit,
	http.StatusInternalServerError: anthropic.ErrAPI,
	529:                            anthropic.ErrOverloaded,
}

func (f *Fault) validate() error {
	switch {
	case f.Delay < 0 || time.Duration(f.Delay) > maxFaultDelay:
		return fmt.Errorf("delay must be between 0 and %s", maxFaultDelay)
	case f.Status != 0 && faultStatus[f.Status] == "":
		return errors.New("status must be 429, 500, or 529")
	case f.DropAfterEvents != nil && *f.DropAfterEvents < 0:
		return errors.New("drop_after_events must not be negative")
	case f.DropMidEvent && f.DropAfterEvents == nil:
		return errors.New("drop_mid_event needs drop_after_events")
	case f.Count < 0:
		return errors.New("count must not be negative")
	}
	return nil
}

// faults holds the active fault. It is safe for concurrent use.
type faults struct {
	mu     sync.Mutex
	active *Fault
}

func (fs *faults) set(f *Fault) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.active = f
}

func (fs *faults) get() *Fault {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.active == nil {
		return nil
	}
	f := *fs.active
	return &f
}

// take returns the fault for one request and counts the use.
func (fs *faults) take() Fault {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.active == nil {
		return Fault{}
	}
	f := *fs.active
	if fs.active.Count > 0 {
		fs.active.Count--
		if fs.active.Count == 0 {
			fs.active = nil
		}
	}
	return f
}

// handleFaults serves GET, POST, and DELETE on /_faults.
func (s *Server) handleFaults(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f := s.faults.get()
		if f == nil {
			f = &Fault{}
		}
		writeJSON(w, http.StatusOK, f)
	case http.MethodPost:
		var f Fault
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, "fault: "+err.Error(), "")
			return
		}
		if err := f.validate(); err != nil {
			writeError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, "fault: "+err.Error(), "")
			return
		}
		s.faults.set(&f)
		s.log.InfoContext(r.Context(), "fault set", "fault", f)
		writeJSON(w, http.StatusOK, &f)
	case http.MethodDelete:
		s.faults.set(nil)
		s.log.InfoContext(r.Context(), "fault cleared")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, anthropic.ErrInvalidRequest, "method not allowed", "")
	}
}
