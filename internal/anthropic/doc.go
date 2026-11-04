// Package anthropic holds the parts of the Anthropic Messages API wire format
// that the gateway reads or writes: a server-sent events (SSE) reader and
// writer, the stream event payloads, and the error body.
//
// The gateway forwards upstream events as the exact bytes it received
// ([Event.Raw]) and decodes only the fields it needs ([ParseStreamEvent]).
// Fields and event types that this package does not know pass through
// unchanged.
package anthropic
