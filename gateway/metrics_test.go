package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// metricValue returns the value of the series of name with exactly labels:
// the value of a counter or gauge, or the sample count of a histogram. It
// returns 0 when the series does not exist.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !sameLabels(m.GetLabel(), labels) {
				continue
			}
			switch {
			case m.Counter != nil:
				return m.GetCounter().GetValue()
			case m.Gauge != nil:
				return m.GetGauge().GetValue()
			case m.Histogram != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

func sameLabels(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, l := range got {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

// streamUsage merges the usage reports of an SSE body, as a client would.
func streamUsage(t *testing.T, body []byte) audit.Usage {
	t.Helper()
	var u anthropic.Usage
	for _, e := range readEvents(t, body) {
		se, err := anthropic.ParseStreamEvent(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		if se.Message != nil {
			u.Merge(se.Message.Usage)
		}
		if se.Usage != nil {
			u.Merge(*se.Usage)
		}
	}
	return auditUsage(u)
}

func TestAuditEvents(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		fault   string
		body    string
		hdr     http.Header
		status  int
		outcome audit.Outcome
		errType string
	}{
		{"text ok", upstreamKey, "", textBody, keyHeader(), 200, audit.OutcomeOK, ""},
		{"stream ok", upstreamKey, "", streamBody, keyHeader(), 200, audit.OutcomeOK, ""},
		{"no key", upstreamKey, "", textBody, http.Header{}, 401, audit.OutcomeUnauthorized, anthropic.ErrAuthentication},
		{"bad json", upstreamKey, "", `{`, keyHeader(), 400, audit.OutcomeInvalid, anthropic.ErrInvalidRequest},
		{"text 429", upstreamKey, `{"status":429}`, textBody, keyHeader(), 429, audit.OutcomeUpstreamError, anthropic.ErrRateLimit},
		{"stream 529", upstreamKey, `{"status":529}`, streamBody, keyHeader(), 529, audit.OutcomeUpstreamError, anthropic.ErrOverloaded},
		{"stream drop", upstreamKey, `{"drop_after_events":3}`, streamBody, keyHeader(), 200, audit.OutcomeIncomplete, anthropic.ErrAPI},
		{"wrong upstream key", "sk-ant-test-wrong", "", textBody, keyHeader(), 502, audit.OutcomeGatewayError, anthropic.ErrAPI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStack(t, tt.key)
			if tt.fault != "" {
				s.setFault(t, tt.fault)
			}
			resp := do(t, s.gateway.URL+"/v1/messages", tt.body, tt.hdr)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			ev := s.event(t)
			stream := strings.Contains(tt.body, `"stream":true`)
			if ev.Outcome != tt.outcome || ev.Status != tt.status || ev.ErrorType != tt.errType || ev.Stream != stream {
				t.Errorf("event outcome %q status %d error %q stream %v; want %q %d %q %v",
					ev.Outcome, ev.Status, ev.ErrorType, ev.Stream, tt.outcome, tt.status, tt.errType, stream)
			}
			if ev.RequestID == "" || ev.RequestID != resp.Header.Get(RequestIDHeader) {
				t.Errorf("event request ID %q, header %q", ev.RequestID, resp.Header.Get(RequestIDHeader))
			}
			if ev.Time.IsZero() || ev.Duration <= 0 {
				t.Errorf("event time %v, duration %v", ev.Time, ev.Duration)
			}
			wantTeam := "red"
			if tt.outcome == audit.OutcomeUnauthorized {
				wantTeam = ""
			}
			if ev.Team != wantTeam {
				t.Errorf("event team = %q, want %q", ev.Team, wantTeam)
			}
			if wantTeam != "" && (ev.User != "alice" || ev.KeyID != auth.HashKey(gatewayKey)[:12]) {
				t.Errorf("event user %q key ID %q", ev.User, ev.KeyID)
			}
			if tt.status != 401 && tt.status != 400 && tt.status != 502 && !strings.HasPrefix(ev.UpstreamRequestID, "req_mock_") {
				t.Errorf("upstream request ID = %q", ev.UpstreamRequestID)
			}
			if tt.outcome == audit.OutcomeIncomplete && ev.Usage.InputTokens == 0 {
				t.Errorf("incomplete stream lost the message_start usage: %+v", ev.Usage)
			}
			labels := map[string]string{"outcome": string(tt.outcome), "stream": map[bool]string{true: "true", false: "false"}[stream]}
			if got := metricValue(t, s.reg, "tokenwarden_requests_total", labels); got != 1 {
				t.Errorf("requests_total%v = %v, want 1", labels, got)
			}
			if got := metricValue(t, s.reg, "tokenwarden_request_duration_seconds", map[string]string{"stream": labels["stream"]}); got != 1 {
				t.Errorf("duration count = %v, want 1", got)
			}
		})
	}
}

func TestAuditUsage(t *testing.T) {
	for _, body := range []string{textBody, streamBody} {
		s := newStack(t, upstreamKey)
		direct := do(t, s.mock.URL+"/v1/messages", body, http.Header{"X-Api-Key": {upstreamKey}})
		var want audit.Usage
		if strings.Contains(body, `"stream":true`) {
			want = streamUsage(t, direct.Body)
		} else {
			var m struct {
				Usage anthropic.Usage `json:"usage"`
			}
			if err := json.Unmarshal(direct.Body, &m); err != nil {
				t.Fatal(err)
			}
			want = auditUsage(m.Usage)
		}
		if want.InputTokens == 0 || want.OutputTokens == 0 {
			t.Fatalf("mock reported no usage: %+v", want)
		}
		do(t, s.gateway.URL+"/v1/messages", body, keyHeader())
		if ev := s.event(t); ev.Usage != want {
			t.Errorf("event usage = %+v, want %+v", ev.Usage, want)
		}
		for kind, n := range map[string]int64{"input": want.InputTokens, "output": want.OutputTokens} {
			if got := metricValue(t, s.reg, "tokenwarden_tokens_total", map[string]string{"team": "red", "kind": kind}); got != float64(n) {
				t.Errorf("tokens_total{%s} = %v, want %d", kind, got, n)
			}
		}
		if got := metricValue(t, s.reg, "tokenwarden_streams_active", map[string]string{}); got != 0 {
			t.Errorf("streams_active = %v after the reply, want 0", got)
		}
	}
}

func TestCaptureLimit(t *testing.T) {
	c := &capture{limit: 8}
	if _, err := io.Copy(c, bytes.NewReader([]byte("12345678"))); err != nil || c.over || c.buf.String() != "12345678" {
		t.Fatalf("at the limit: buf %q over %v err %v", c.buf.String(), c.over, err)
	}
	n, err := c.Write([]byte("9"))
	if n != 1 || err != nil || !c.over || c.buf.Len() != 0 {
		t.Fatalf("past the limit: n %d err %v over %v len %d", n, err, c.over, c.buf.Len())
	}
}

func TestRegisterTwiceFails(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := Config{UpstreamURL: "http://127.0.0.1:8081", UpstreamKey: upstreamKey, Authenticator: newAuth(t), Registerer: reg}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	_, err := New(cfg)
	if are := (prometheus.AlreadyRegisteredError{}); !errors.As(err, &are) {
		t.Fatalf("second New: err = %v, want AlreadyRegisteredError", err)
	}
}
