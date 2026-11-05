// Package mock is a mock Anthropic Messages API upstream for tests and demos.
//
// The mock streams seeded lorem ipsum at a set pace, can reply with a
// tool_use block for a tool that the request declares, and applies faults
// that a caller sets through the /_faults endpoint. A replay of the same
// request body gives the same reply. The mock counts one word, or one
// partial_json chunk, as one token, so the usage counts it reports match the
// content it sends.
package mock
