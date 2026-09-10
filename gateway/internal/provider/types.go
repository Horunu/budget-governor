// Package provider defines the gateway's normalized view of an LLM call
// and the adapters that translate it to/from each upstream provider's
// native wire format (OpenAI Chat Completions / Responses, Anthropic
// Messages, and a deterministic Mock provider used for local dev, the
// smoke test, and the load test).
package provider

import (
	"context"
	"io"
)

// Message is a normalized chat message, independent of provider wire format.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content string `json:"content"`
}

// Request is the gateway's normalized representation of an inbound call,
// built from whichever client-facing shape was used (OpenAI-compatible
// Chat Completions body is the primary client contract; see
// gateway/internal/proxy for the translation from HTTP).
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	System      string    `json:"system,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	Stream      bool      `json:"stream,omitempty"`

	// Populated by the gateway before dispatch, not by the client.
	TenantID string `json:"-"`
	AgentID  string `json:"-"`
	TaskID   string `json:"-"`
	TraceID  string `json:"-"`
}

// Usage carries actual token accounting as reported (or, for the mock
// provider, deterministically simulated) by the upstream call.
type Usage struct {
	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
}

// Response is the gateway's normalized representation of a completed
// (non-streaming) upstream call.
type Response struct {
	ID           string  `json:"id"`
	Model        string  `json:"model"`
	Content      string  `json:"content"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
	CostUSD      float64 `json:"cost_usd"`
	LatencyMs    int64   `json:"latency_ms"`
}

// StreamChunk is one normalized server-sent event emitted while streaming.
// Done is set on the terminal chunk, at which point Usage is populated
// with the final token accounting (providers report usage only once the
// stream completes).
type StreamChunk struct {
	DeltaContent string
	Done         bool
	FinishReason string
	Usage        Usage
}

// ProviderError normalizes upstream error responses (OpenAI's
// {"error":{...}} and Anthropic's {"type":"error","error":{...}} shapes)
// into one structure the gateway can log/meter/return consistently.
type ProviderError struct {
	StatusCode int
	Type       string // e.g. "rate_limit_exceeded", "invalid_request_error", "overloaded_error"
	Message    string
	RetryAfter int // seconds, 0 if not provided
}

func (e *ProviderError) Error() string {
	return e.Type + ": " + e.Message
}

// Provider is implemented by every upstream adapter (OpenAI, Anthropic,
// Mock). Complete is used for the non-streaming path; Stream is used for
// SSE passthrough. Both must return accurate Usage so the gateway can
// reconcile the token-bucket estimate against actual consumption.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, w StreamWriter) error
}

// StreamWriter receives normalized chunks as they arrive from the
// upstream provider; the proxy layer implements this to translate chunks
// back into the client-facing SSE format.
type StreamWriter interface {
	Write(chunk StreamChunk) error
}

// FlushWriter is satisfied by http.ResponseWriter + http.Flusher; kept as
// a narrow interface here so provider adapters don't import net/http.
type FlushWriter interface {
	io.Writer
	Flush()
}
