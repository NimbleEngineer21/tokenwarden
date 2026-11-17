// Package cel is a default-deny [policy.Policy] that loads rules written in
// CEL (https://cel.dev) from a YAML file and reloads them when the file
// changes.
//
// A policy file looks like this:
//
//	version: 1
//	mode: enforce # or observe
//	rules:
//	  - name: no-opus-for-interns
//	    match: {teams: [interns]}
//	    expr: request.model.startsWith("claude-opus")
//	    effect: deny
//	    reason: Opus models are for staff only.
//	  - name: everyone
//	    effect: allow
//
// A rule matches a request when its match selectors and its expression are
// both true. An empty selector list matches any value, and an empty
// expression is true. The first deny rule that matches, in file order,
// denies the request. Otherwise the first allow rule that matches allows
// it. A request that no rule matches is denied.
//
// Expressions see these variables:
//
//	request.team       string
//	request.user       string
//	request.model      string
//	request.max_tokens int
//	request.tools      list(string), the names of the tool definitions
//	grants             list(string), the active break-glass grants
//
// An expression must have the type bool. The engine checks every
// expression when it loads the file, so a typo such as request.modl fails
// the load, not a request. A deny rule whose expression fails at run time
// counts as a match, so the engine fails closed. An allow rule whose
// expression fails does not match.
//
// In observe mode the engine allows every request. When the rules would
// deny a request, it logs the decision and sets
// [policy.Decision.WouldDeny].
package cel
