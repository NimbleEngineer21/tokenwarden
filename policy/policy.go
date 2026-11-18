// Package policy defines the [Policy] that a gateway asks before it forwards
// a request, and the [Input] and [Decision] that pass through it.
//
// A policy is default deny: a request that no rule allows is denied. The
// policy/cel subpackage holds an implementation that loads rules written in
// CEL from a YAML file.
package policy

import "context"

// Input holds the request attributes that a [Policy] decides on. It never
// holds the prompt or any key.
type Input struct {
	// Team is the team that owns the gateway key.
	Team string
	// User is the user that the gateway key belongs to. It can be empty.
	User string
	// Model is the model that the request names.
	Model string
	// MaxTokens is the max_tokens value of the request.
	MaxTokens int64
	// Tools holds the names of the tool definitions in the request. MCP
	// tools arrive as client tools with names such as
	// mcp__github__create_issue, so rules on names also cover them.
	Tools []string
	// Grants holds the names of the break-glass grants that are active for
	// the team.
	Grants []string
}

// Decision is the result of a [Policy].
type Decision struct {
	// Allow reports whether the gateway may forward the request.
	Allow bool
	// Rule is the name of the rule that decided. It is empty when no rule
	// matched and the default deny applied.
	Rule string
	// Reason explains the decision. It is safe to show to the client.
	Reason string
	// WouldDeny reports that the policy runs in observe mode and would
	// have denied the request. Allow is then true.
	WouldDeny bool
	// Revision names the version of the rules that decided, so that an
	// audit record shows which file was in force. It can be empty.
	Revision string
}

// Policy decides whether a request may go to the upstream.
type Policy interface {
	// Decide returns the decision for in. An error means that the policy
	// could not decide; the caller must then deny the request.
	Decide(ctx context.Context, in Input) (Decision, error)
}
