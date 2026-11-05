package mock

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// eventsPerFlush parses each flush on its own and fails unless it holds
// exactly one event. It returns the events in order.
func eventsPerFlush(t *testing.T, rec *flushRecorder) []anthropic.StreamEvent {
	t.Helper()
	var out []anthropic.StreamEvent
	for i, fl := range rec.flushes {
		res := parseStream(t, fl.data)
		if len(res.events) != 1 {
			t.Fatalf("flush %d holds %d events, want 1", i, len(res.events))
		}
		out = append(out, res.events[0])
	}
	return out
}

func TestStreamPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ttft = 300 * time.Millisecond
		const perToken = 25 * time.Millisecond // 40 tokens per second
		s := New(Config{TTFT: ttft, TokensPerSecond: 40})
		rec := serve(t, s, msgBody("pace me", 200, true, ""))

		evs := eventsPerFlush(t, rec)
		if len(evs) < 7 {
			t.Fatalf("got %d events", len(evs))
		}
		want := ttft
		for i, ev := range evs {
			if ev.Type == anthropic.EventContentBlockDelta {
				want += time.Duration(len(strings.Fields(ev.Delta.Text))) * perToken
			}
			if got := rec.flushes[i].at; got != want {
				t.Fatalf("event %d (%s) flushed at %v, want %v", i, ev.Type, got, want)
			}
		}
	})
}

func TestStreamJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const jitter = 0.5
		s := New(Config{TokensPerSecond: 40, Jitter: jitter})
		rec := serve(t, s, msgBody("jitter me", 200, true, ""))
		evs := eventsPerFlush(t, rec)
		gaps := map[time.Duration]bool{}
		for i, ev := range evs {
			if ev.Type != anthropic.EventContentBlockDelta {
				continue
			}
			base := time.Duration(len(strings.Fields(ev.Delta.Text))) * 25 * time.Millisecond
			gap := rec.flushes[i].at - rec.flushes[i-1].at
			lo := time.Duration(float64(base) * (1 - jitter))
			hi := time.Duration(float64(base) * (1 + jitter))
			if gap < lo || gap > hi {
				t.Fatalf("event %d gap %v outside [%v, %v]", i, gap, lo, hi)
			}
			gaps[gap] = true
		}
		if len(gaps) < 3 {
			t.Fatalf("jitter gave only %d distinct gaps", len(gaps))
		}
	})
}

func TestNonStreamWaitsForWholeReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{TTFT: 300 * time.Millisecond, TokensPerSecond: 40})
		start := time.Now()
		rec := serve(t, s, msgBody("whole", 200, false, ""))
		elapsed := time.Since(start)
		res := parseStream(t, serve(t, New(Config{}), msgBody("whole", 200, true, "")).body())
		want := 300*time.Millisecond + time.Duration(res.usage.OutputTokens)*25*time.Millisecond
		if elapsed != want {
			t.Fatalf("reply after %v, want %v", elapsed, want)
		}
		if len(rec.flushes) != 0 {
			t.Fatalf("non-stream reply flushed %d times", len(rec.flushes))
		}
	})
}

func TestStreamStopsOnDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{TokensPerSecond: 10})
		s.faults.set(&Fault{LongReply: true}) // 1000 tokens take 100s
		ctx, cancel := context.WithCancel(t.Context())
		rec := newFlushRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.ServeHTTP(rec, newRequest(ctx, msgBody("long", 1000, true, "")))
		}()
		time.Sleep(time.Second)
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("handler still runs after the client left")
		}
		if strings.Contains(rec.body(), anthropic.EventMessageStop) {
			t.Fatal("stream completed although the client left")
		}
	})
}

func TestDelayFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{TTFT: 300 * time.Millisecond})
		s.faults.set(&Fault{Delay: Duration(2 * time.Second), Count: 1})
		rec := serve(t, s, msgBody("slow", 50, true, ""))
		if got := rec.flushes[0].at; got != 2300*time.Millisecond {
			t.Fatalf("first event at %v, want 2.3s", got)
		}
		rec = serve(t, s, msgBody("slow", 50, true, ""))
		if got := rec.flushes[0].at; got != 300*time.Millisecond {
			t.Fatalf("after the fault: first event at %v, want 300ms", got)
		}
	})
}

func TestDelayBeforeStatusFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Config{})
		s.faults.set(&Fault{Delay: Duration(time.Second), Status: 529})
		start := time.Now()
		rec := serve(t, s, msgBody("x", 10, true, ""))
		if time.Since(start) != time.Second || rec.code != 529 {
			t.Fatalf("status %d after %v, want 529 after 1s", rec.code, time.Since(start))
		}
	})
}
