package cel_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NimbleEngineer21/tokenwarden/policy"
	"github.com/NimbleEngineer21/tokenwarden/policy/cel"
)

// lockedBuffer is a log sink that the engine's goroutines and the test can
// share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// load writes data to a policy file and loads it. The returned buffer holds
// the engine's log.
func load(t *testing.T, data string) (*cel.Engine, string, *lockedBuffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	writeFile(t, path, data)
	logs := &lockedBuffer{}
	e, err := cel.Load(path, cel.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	return e, path, logs
}

const rulesYAML = `version: 1
mode: %s
rules:
  - name: staff-only-opus
    match: {teams: [interns]}
    expr: request.model.startsWith("claude-opus")
    effect: deny
    reason: Opus models are for staff only.
  - name: no-fs-write
    expr: '"fs_write" in request.tools && !("tool:fs_write" in grants)'
    effect: deny
    reason: fs_write needs a grant.
  - name: no-github-mcp-for-ann
    match: {users: [ann]}
    expr: request.tools.exists(t, t.startsWith("mcp__github__"))
    effect: deny
    reason: Ann may not use the GitHub MCP server.
  - name: small-requests
    expr: request.max_tokens <= 4096
    effect: allow
    reason: small
  - name: red-team
    match: {teams: [red]}
    effect: allow
`

func TestDecide(t *testing.T) {
	e, _, _ := load(t, strings.ReplaceAll(rulesYAML, "%s", "enforce"))
	tests := []struct {
		name string
		in   policy.Input
		want policy.Decision
	}{
		{
			"first allow rule decides",
			policy.Input{Team: "red", Model: "claude-opus-5", MaxTokens: 1000},
			policy.Decision{Allow: true, Rule: "small-requests", Reason: "small"},
		},
		{
			"later allow rule",
			policy.Input{Team: "red", MaxTokens: 10000},
			policy.Decision{Allow: true, Rule: "red-team"},
		},
		{
			"default deny",
			policy.Input{Team: "blue", MaxTokens: 10000},
			policy.Decision{Reason: cel.ReasonNoRule},
		},
		{
			"deny beats an allow listed before it",
			policy.Input{Team: "red", MaxTokens: 1000, Tools: []string{"fs_write"}},
			policy.Decision{Rule: "no-fs-write", Reason: "fs_write needs a grant."},
		},
		{
			"grant lifts the deny",
			policy.Input{Team: "red", MaxTokens: 1000, Tools: []string{"fs_write"}, Grants: []string{"tool:fs_write"}},
			policy.Decision{Allow: true, Rule: "small-requests", Reason: "small"},
		},
		{
			"team selector",
			policy.Input{Team: "interns", Model: "claude-opus-5", MaxTokens: 1000},
			policy.Decision{Rule: "staff-only-opus", Reason: "Opus models are for staff only."},
		},
		{
			"first deny in file order wins",
			policy.Input{Team: "interns", Model: "claude-opus-5", MaxTokens: 1000, Tools: []string{"fs_write"}},
			policy.Decision{Rule: "staff-only-opus", Reason: "Opus models are for staff only."},
		},
		{
			"user selector and MCP tool name",
			policy.Input{Team: "red", User: "ann", MaxTokens: 1000, Tools: []string{"mcp__github__create_issue"}},
			policy.Decision{Rule: "no-github-mcp-for-ann", Reason: "Ann may not use the GitHub MCP server."},
		},
		{
			"other user may use the MCP tool",
			policy.Input{Team: "red", User: "bob", MaxTokens: 1000, Tools: []string{"mcp__github__create_issue"}},
			policy.Decision{Allow: true, Rule: "small-requests", Reason: "small"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Decide(t.Context(), tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want.Revision = e.Revision(); got != tt.want {
				t.Fatalf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecideNoRulesDenies(t *testing.T) {
	e, _, _ := load(t, "version: 1\nrules: []\n")
	got, err := e.Decide(t.Context(), policy.Input{Team: "red"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (policy.Decision{Reason: cel.ReasonNoRule, Revision: e.Revision()}); got != want {
		t.Fatalf("Decide() = %+v, want %+v", got, want)
	}
}

func TestDecideObserve(t *testing.T) {
	e, _, logs := load(t, strings.ReplaceAll(rulesYAML, "%s", "observe"))

	got, err := e.Decide(t.Context(), policy.Input{Team: "red", MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if want := (policy.Decision{Allow: true, Rule: "small-requests", Reason: "small", Revision: e.Revision()}); got != want {
		t.Fatalf("allowed: Decide() = %+v, want %+v", got, want)
	}
	if strings.Contains(logs.String(), "would deny") {
		t.Fatalf("an allowed request was logged as would-deny:\n%s", logs)
	}

	got, err = e.Decide(t.Context(), policy.Input{Team: "interns", User: "ann", Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	want := policy.Decision{Allow: true, Rule: "staff-only-opus", Reason: "Opus models are for staff only.", WouldDeny: true, Revision: e.Revision()}
	if got != want {
		t.Fatalf("denied: Decide() = %+v, want %+v", got, want)
	}
	for _, s := range []string{`msg="policy would deny"`, "team=interns", "user=ann", "model=claude-opus-5", "rule=staff-only-opus"} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("log does not contain %q:\n%s", s, logs)
		}
	}

	got, err = e.Decide(t.Context(), policy.Input{Team: "blue", MaxTokens: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if want := (policy.Decision{Allow: true, Reason: cel.ReasonNoRule, WouldDeny: true, Revision: e.Revision()}); got != want {
		t.Fatalf("default deny: Decide() = %+v, want %+v", got, want)
	}
}

func TestDecideEvalError(t *testing.T) {
	// 100 tools make the triple loop cost far more than the cost limit.
	many := make([]string, 100)
	for i := range many {
		many[i] = "t"
	}
	tests := []struct {
		name   string
		effect string
		expr   string
		in     policy.Input
		want   policy.Decision
	}{
		{
			"deny rule fails closed",
			"deny", "request.max_tokens / 0 > 1",
			policy.Input{},
			policy.Decision{Rule: "broken", Reason: cel.ReasonEvalError},
		},
		{
			"allow rule does not match",
			"allow", "request.tools[3] == 'x'",
			policy.Input{},
			policy.Decision{Reason: cel.ReasonNoRule},
		},
		{
			"cost limit fails closed",
			"deny", "request.tools.all(a, request.tools.all(b, request.tools.all(c, a == b)))",
			policy.Input{Tools: many},
			policy.Decision{Rule: "broken", Reason: cel.ReasonEvalError},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, _ := load(t, "version: 1\nrules:\n- name: broken\n  effect: "+tt.effect+
				"\n  reason: r\n  expr: \""+tt.expr+"\"\n- name: all\n  effect: allow\n  expr: 'false'\n")
			got, err := e.Decide(t.Context(), tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want.Revision = e.Revision(); got != tt.want {
				t.Fatalf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecideCanceled(t *testing.T) {
	e, _, _ := load(t, "version: 1\nrules:\n- name: all\n  effect: allow\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Decide(ctx, policy.Input{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Decide() error = %v, want context.Canceled", err)
	}
}

func TestLoadRejectsBadExpressions(t *testing.T) {
	tests := []struct {
		name, expr, want string
	}{
		{"unknown field", "request.modl == 'x'", "modl"},
		{"unknown variable", "req.model == 'x'", "undeclared reference to 'req'"},
		{"not bool", "request.max_tokens + 1", "not bool"},
		{"syntax", "request.model ==", "Syntax error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.yaml")
			writeFile(t, path, "version: 1\nrules:\n- name: r\n  effect: allow\n  expr: \""+tt.expr+"\"\n")
			_, err := cel.Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), `rule 1 "r"`) {
				t.Fatalf("Load() error = %v, want it to name rule 1 and contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := cel.Load(filepath.Join(t.TempDir(), "none.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want os.ErrNotExist", err)
	}
}
