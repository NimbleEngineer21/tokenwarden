package cel

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	celgo "cel.dev/cel-go/cel"

	"github.com/NimbleEngineer21/tokenwarden/policy"
)

// ReasonNoRule is the [policy.Decision] reason when no rule matches a
// request and the default deny applies.
const ReasonNoRule = "no rule allows this request"

// ReasonEvalError is the [policy.Decision] reason when the expression of a
// deny rule fails at run time.
const ReasonEvalError = "the policy rule could not be evaluated"

// Engine is a [policy.Policy] that holds the rules of one policy file. It is
// safe for concurrent use. [Engine.Reload] and [Engine.Watch] swap in a new
// version of the rules without a stop in service.
type Engine struct {
	path string
	env  *celgo.Env
	log  *slog.Logger

	reloadMu sync.Mutex
	rules    atomic.Pointer[ruleSet]
}

// Option configures an [Engine].
type Option func(*Engine)

// WithLogger sets the logger for reloads and observe-mode decisions. The
// default is [slog.Default].
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) { e.log = l }
}

// Load reads and compiles the policy file at path. It returns an error if
// the file is not valid, so that a bad policy stops the gateway at start.
func Load(path string, opts ...Option) (*Engine, error) {
	env, err := newEnv()
	if err != nil {
		return nil, fmt.Errorf("policy: cel environment: %w", err)
	}
	e := &Engine{path: path, env: env, log: slog.Default()}
	for _, o := range opts {
		o(e)
	}
	if err := e.Reload(); err != nil {
		return nil, err
	}
	return e, nil
}

// Revision returns the SHA-256 of the policy file that the engine uses now,
// in hex.
func (e *Engine) Revision() string { return e.rules.Load().revision }

// Reload reads and compiles the policy file again. If the new file is not
// valid, the engine keeps the rules that it has and Reload returns the
// error.
func (e *Engine) Reload() error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	data, err := os.ReadFile(e.path)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	return e.swap(data)
}

// swap compiles data and makes it the active rule set. The caller holds
// reloadMu.
func (e *Engine) swap(data []byte) error {
	rs, err := compile(e.env, data)
	if err != nil {
		return fmt.Errorf("policy: %s: %w", e.path, err)
	}
	e.rules.Store(rs)
	e.log.Info("policy loaded", "path", e.path, "revision", rs.revision,
		"mode", string(rs.mode), "rules", len(rs.rules))
	return nil
}

// Watch polls the policy file every interval and reloads it when its
// content changes. It logs a file that is not valid and keeps the rules
// that it has. Watch returns when ctx is done. The interval must be
// positive.
//
// Watch compares content, not modification times, so it also sees a file
// that a Kubernetes ConfigMap update replaces through a symlink.
func (e *Engine) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var failed []byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		data, err := os.ReadFile(e.path)
		if err != nil {
			e.log.ErrorContext(ctx, "policy reload failed", "path", e.path, "error", err)
			continue
		}
		// A file that failed once is logged once, not on every tick.
		if bytes.Equal(data, failed) {
			continue
		}
		if err := e.reloadIfChanged(data); err != nil {
			failed = data
			e.log.ErrorContext(ctx, "policy reload failed", "path", e.path, "error", err)
			continue
		}
		failed = nil
	}
}

func (e *Engine) reloadIfChanged(data []byte) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	if e.rules.Load().revision == revisionOf(data) {
		return nil
	}
	return e.swap(data)
}

// Decide implements [policy.Policy].
func (e *Engine) Decide(ctx context.Context, in policy.Input) (policy.Decision, error) {
	rs := e.rules.Load()
	d, err := e.evaluate(ctx, rs, in)
	if err != nil {
		return policy.Decision{}, err
	}
	if d.Allow || rs.mode != ModeObserve {
		return d, nil
	}
	e.log.WarnContext(ctx, "policy would deny", "team", in.Team, "user", in.User,
		"model", in.Model, "rule", d.Rule, "reason", d.Reason, "revision", rs.revision)
	d.Allow, d.WouldDeny = true, true
	return d, nil
}

func (e *Engine) evaluate(ctx context.Context, rs *ruleSet, in policy.Input) (policy.Decision, error) {
	vars := map[string]any{
		"request": request{
			Team:      in.Team,
			User:      in.User,
			Model:     in.Model,
			MaxTokens: in.MaxTokens,
			Tools:     nonNil(in.Tools),
		},
		"grants": nonNil(in.Grants),
	}
	var allow *rule
	for i := range rs.rules {
		r := &rs.rules[i]
		if !r.match.matches(in.Team, in.User) {
			continue
		}
		out, _, err := r.prg.ContextEval(ctx, vars)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return policy.Decision{}, fmt.Errorf("policy: %w", ctxErr)
		}
		if err != nil {
			if r.effect == EffectDeny {
				e.log.WarnContext(ctx, "policy rule failed; denying", "rule", r.name, "error", err)
				return policy.Decision{Rule: r.name, Reason: ReasonEvalError}, nil
			}
			continue
		}
		if out.Value() != true {
			continue
		}
		if r.effect == EffectDeny {
			return policy.Decision{Rule: r.name, Reason: r.reason}, nil
		}
		if allow == nil {
			allow = r
		}
	}
	if allow != nil {
		return policy.Decision{Allow: true, Rule: allow.name, Reason: allow.reason}, nil
	}
	return policy.Decision{Reason: ReasonNoRule}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
