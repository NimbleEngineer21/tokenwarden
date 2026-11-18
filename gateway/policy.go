package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/NimbleEngineer21/tokenwarden/audit"
	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
	"github.com/NimbleEngineer21/tokenwarden/policy"
)

// Messages of the policy errors.
const (
	errMsgPolicyDenied      = "the policy denies this request"
	errMsgPolicyUnavailable = "the policy is unavailable"
	errMsgPolicyFields      = "max_tokens must be an integer and tools a list of objects with a string name"
)

// Results of a policy decision, the result label of the decision metric.
const (
	policyAllow     = "allow"
	policyDeny      = "deny"
	policyWouldDeny = "would_deny"
	policyError     = "error"
)

// decide asks the policy about the request. On a deny or a failure it sends
// the error reply, notes it in ev, and returns false.
func (g *gateway) decide(ctx context.Context, w http.ResponseWriter, ev *audit.Event, body []byte, log *slog.Logger) bool {
	var p struct {
		MaxTokens int64 `json:"max_tokens"`
		Tools     []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	// The policy cannot judge fields that it cannot read, so the request
	// fails here and not upstream.
	if err := json.Unmarshal(body, &p); err != nil {
		fail(w, ev, audit.OutcomeInvalid, http.StatusBadRequest, anthropic.ErrInvalidRequest, errMsgPolicyFields)
		return false
	}
	in := policy.Input{Team: ev.Team, User: ev.User, Model: ev.Model, MaxTokens: p.MaxTokens}
	for _, t := range p.Tools {
		in.Tools = append(in.Tools, t.Name)
	}
	d, err := g.policy.Decide(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			ev.Outcome = audit.OutcomeCanceled
			return false
		}
		g.metrics.decisions.WithLabelValues(policyError, "").Inc()
		log.ErrorContext(ctx, "policy failed", "err", err)
		fail(w, ev, audit.OutcomeGatewayError, http.StatusInternalServerError, anthropic.ErrAPI, errMsgPolicyUnavailable)
		return false
	}
	ev.Policy = audit.Policy{Rule: d.Rule, Reason: d.Reason, WouldDeny: d.WouldDeny, Revision: d.Revision}
	switch {
	case !d.Allow:
		g.metrics.decisions.WithLabelValues(policyDeny, d.Rule).Inc()
		msg := d.Reason
		if msg == "" {
			msg = errMsgPolicyDenied
		}
		fail(w, ev, audit.OutcomePolicyDenied, http.StatusForbidden, anthropic.ErrPermission, msg)
		return false
	case d.WouldDeny:
		g.metrics.decisions.WithLabelValues(policyWouldDeny, d.Rule).Inc()
	default:
		g.metrics.decisions.WithLabelValues(policyAllow, d.Rule).Inc()
	}
	return true
}
