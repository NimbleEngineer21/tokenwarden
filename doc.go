// Package tokenwarden governs LLM traffic to the Anthropic Messages API.
//
// It enforces token budgets and concurrency caps per team across many gateway
// replicas, applies default-deny policy to models and tools, and records an
// audit event for every request. The subpackages hold the public API; the
// cmd directory holds a reference gateway and its tools.
package tokenwarden
