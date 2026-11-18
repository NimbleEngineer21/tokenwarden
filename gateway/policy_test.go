package gateway

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
	"github.com/NimbleEngineer21/tokenwarden/policy"
	"github.com/NimbleEngineer21/tokenwarden/policy/cel"
)

// fakePolicy returns a fixed decision and records each input.
type fakePolicy struct {
	d   policy.Decision
	err error

	mu  sync.Mutex
	ins []policy.Input
}

func (p *fakePolicy) Decide(_ context.Context, in policy.Input) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ins = append(p.ins, in)
	return p.d, p.err
}

func (p *fakePolicy) inputs() []policy.Input {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ins)
}

func withPolicy(p policy.Policy) func(*Config) {
	return func(c *Config) { c.Policy = p }
}

func decisions(t *testing.T, s *stack, result, rule string) float64 {
	t.Helper()
	return metricValue(t, s.reg, "tokenwarden_policy_decisions_total", map[string]string{"result": result, "rule": rule})
}

func TestPolicyInput(t *testing.T) {
	p := &fakePolicy{d: policy.Decision{Allow: true}}
	s := newStack(t, upstreamKey, withPolicy(p))
	body := `{"model":"claude-test","max_tokens":77,"tools":[{"name":"get_weather","input_schema":{}},` +
		`{"type":"web_search_20250305","name":"web_search"},{"name":"mcp__github__create_issue"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	if resp := do(t, s.gateway.URL+"/v1/messages", body, keyHeader()); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, resp.Body)
	}
	s.event(t)
	ins := p.inputs()
	if len(ins) != 1 {
		t.Fatalf("Decide called %d times, want 1", len(ins))
	}
	want := policy.Input{
		Team: "red", User: "alice", Model: "claude-test", MaxTokens: 77,
		Tools: []string{"get_weather", "web_search", "mcp__github__create_issue"},
	}
	got := ins[0]
	if got.Team != want.Team || got.User != want.User || got.Model != want.Model ||
		got.MaxTokens != want.MaxTokens || !slices.Equal(got.Tools, want.Tools) || got.Grants != nil {
		t.Fatalf("Input = %+v, want %+v", got, want)
	}
}

func TestPolicyAllows(t *testing.T) {
	for _, tt := range []struct {
		name   string
		d      policy.Decision
		result string
	}{
		{"allow", policy.Decision{Allow: true, Rule: "everyone", Revision: "rev1"}, policyAllow},
		{"observe", policy.Decision{Allow: true, Rule: "no-opus", Reason: "no", WouldDeny: true, Revision: "rev1"}, policyWouldDeny},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStack(t, upstreamKey, withPolicy(&fakePolicy{d: tt.d}))
			if resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader()); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body %s", resp.StatusCode, resp.Body)
			}
			ev := s.event(t)
			want := audit.Policy{Rule: tt.d.Rule, Reason: tt.d.Reason, WouldDeny: tt.d.WouldDeny, Revision: tt.d.Revision}
			if ev.Outcome != audit.OutcomeOK || ev.Policy != want {
				t.Fatalf("event outcome %q policy %+v, want ok %+v", ev.Outcome, ev.Policy, want)
			}
			if n := len(s.spy.requests()); n != 1 {
				t.Fatalf("upstream requests = %d, want 1", n)
			}
			if v := decisions(t, s, tt.result, tt.d.Rule); v != 1 {
				t.Fatalf("decisions{%s,%s} = %v, want 1", tt.result, tt.d.Rule, v)
			}
		})
	}
}

func TestPolicyDenies(t *testing.T) {
	for _, tt := range []struct {
		name, reason, msg string
	}{
		{"reason", "Opus models are for staff only.", "Opus models are for staff only."},
		{"no reason", "", errMsgPolicyDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := policy.Decision{Rule: "no-opus", Reason: tt.reason, Revision: "rev2"}
			s, rl := newBudgetStack(t, 100_000, withPolicy(&fakePolicy{d: d}))
			resp := do(t, s.gateway.URL+"/v1/messages", streamBody, keyHeader())
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", resp.StatusCode, resp.Body)
			}
			e := errorBody(t, resp)
			if e.Error.Type != anthropic.ErrPermission || e.Error.Message != tt.msg {
				t.Fatalf("error = %+v, want %s %q", e.Error, anthropic.ErrPermission, tt.msg)
			}
			if strings.Contains(string(resp.Body), "no-opus") {
				t.Fatalf("the reply names the rule: %s", resp.Body)
			}
			ev := s.event(t)
			want := audit.Policy{Rule: "no-opus", Reason: tt.reason, Revision: "rev2"}
			if ev.Outcome != audit.OutcomePolicyDenied || ev.Status != http.StatusForbidden ||
				ev.ErrorType != anthropic.ErrPermission || ev.Policy != want {
				t.Fatalf("event = %+v, want policy_denied 403 %+v", ev, want)
			}
			if n := len(s.spy.requests()); n != 0 {
				t.Fatalf("upstream requests = %d, want 0", n)
			}
			if reserves, _, _ := rl.calls(); len(reserves) != 0 {
				t.Fatalf("reserves = %v, want none before a denied request", reserves)
			}
			if v := decisions(t, s, policyDeny, "no-opus"); v != 1 {
				t.Fatalf("decisions{deny,no-opus} = %v, want 1", v)
			}
		})
	}
}

func TestPolicyFails(t *testing.T) {
	s, rl := newBudgetStack(t, 100_000, withPolicy(&fakePolicy{err: errors.New("broken")}))
	resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader())
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if e := errorBody(t, resp); e.Error.Type != anthropic.ErrAPI || e.Error.Message != errMsgPolicyUnavailable {
		t.Fatalf("error = %+v", e.Error)
	}
	ev := s.event(t)
	if ev.Outcome != audit.OutcomeGatewayError || ev.Policy != (audit.Policy{}) {
		t.Fatalf("event outcome %q policy %+v, want gateway_error and no policy", ev.Outcome, ev.Policy)
	}
	if n := len(s.spy.requests()); n != 0 {
		t.Fatalf("upstream requests = %d, want 0", n)
	}
	if reserves, _, _ := rl.calls(); len(reserves) != 0 {
		t.Fatalf("reserves = %v, want none", reserves)
	}
	if v := decisions(t, s, policyError, ""); v != 1 {
		t.Fatalf("decisions{error} = %v, want 1", v)
	}
}

// blockingPolicy waits in Decide until the request context ends.
type blockingPolicy struct{ started chan struct{} }

func (p blockingPolicy) Decide(ctx context.Context, _ policy.Input) (policy.Decision, error) {
	close(p.started)
	<-ctx.Done()
	return policy.Decision{}, ctx.Err()
}

func TestPolicyClientGone(t *testing.T) {
	p := blockingPolicy{started: make(chan struct{})}
	s := newStack(t, upstreamKey, withPolicy(p))
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.gateway.URL+"/v1/messages", strings.NewReader(textBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = keyHeader()
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-p.started
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("request finished, want it canceled")
	}
	ev := s.event(t)
	if ev.Outcome != audit.OutcomeCanceled || ev.Status != 0 {
		t.Fatalf("event outcome %q status %d, want canceled 0", ev.Outcome, ev.Status)
	}
	if v := decisions(t, s, policyError, ""); v != 0 {
		t.Fatalf("decisions{error} = %v, want 0 for a client that went away", v)
	}
}

func TestPolicyFieldTypes(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-test","max_tokens":"lots","messages":[]}`,
		`{"model":"claude-test","max_tokens":1.5,"messages":[]}`,
		`{"model":"claude-test","max_tokens":10,"tools":"all","messages":[]}`,
		`{"model":"claude-test","max_tokens":10,"tools":[{"name":7}],"messages":[]}`,
	} {
		p := &fakePolicy{d: policy.Decision{Allow: true}}
		s := newStack(t, upstreamKey, withPolicy(p))
		resp := do(t, s.gateway.URL+"/v1/messages", body, keyHeader())
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, resp.StatusCode)
		}
		if e := errorBody(t, resp); e.Error.Type != anthropic.ErrInvalidRequest || e.Error.Message != errMsgPolicyFields {
			t.Fatalf("%s: error = %+v", body, e.Error)
		}
		if ev := s.event(t); ev.Outcome != audit.OutcomeInvalid {
			t.Fatalf("%s: outcome = %q, want invalid", body, ev.Outcome)
		}
		if n := len(p.inputs()); n != 0 {
			t.Fatalf("%s: Decide called %d times, want 0", body, n)
		}
	}
}

func TestNoPolicy(t *testing.T) {
	s := newStack(t, upstreamKey)
	if resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader()); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ev := s.event(t); ev.Policy != (audit.Policy{}) {
		t.Fatalf("policy = %+v, want empty without a policy", ev.Policy)
	}
}

// TestPolicyCEL runs the gateway with the CEL engine and a real file.
func TestPolicyCEL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	file := "version: 1\nrules:\n" +
		"- name: red-no-opus\n  match: {teams: [red]}\n  expr: request.model.startsWith('claude-opus')\n" +
		"  effect: deny\n  reason: Opus models are for staff only.\n" +
		"- name: everyone\n  effect: allow\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := cel.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newStack(t, upstreamKey, withPolicy(e))

	resp := do(t, s.gateway.URL+"/v1/messages", strings.Replace(textBody, "claude-test", "claude-opus-5", 1), keyHeader())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("opus: status = %d, want 403", resp.StatusCode)
	}
	if ev := s.event(t); ev.Policy.Rule != "red-no-opus" || ev.Policy.Revision != e.Revision() {
		t.Fatalf("opus: policy = %+v, want red-no-opus at %s", ev.Policy, e.Revision())
	}
	if resp := do(t, s.gateway.URL+"/v1/messages", textBody, keyHeader()); resp.StatusCode != http.StatusOK {
		t.Fatalf("other model: status = %d, want 200", resp.StatusCode)
	}
	if ev := s.event(t); ev.Policy.Rule != "everyone" {
		t.Fatalf("other model: rule = %q, want everyone", ev.Policy.Rule)
	}
}
