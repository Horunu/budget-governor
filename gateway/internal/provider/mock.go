package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	mrand "math/rand"
	"strings"
	"time"
)

// MockProvider simulates an upstream LLM with realistic variable latency,
// input-length-dependent token counts, and real pricing-table-driven cost
// calculation — with zero network calls. This is what docker-compose runs
// against by default (no API keys required) and what the load test and
// smoke test exercise.
type MockProvider struct {
	MinLatency time.Duration
	MaxLatency time.Duration
	// Now is overridable for deterministic tests.
	Now func() time.Time
}

func NewMockProvider(minLatencyMs, maxLatencyMs int) *MockProvider {
	return &MockProvider{
		MinLatency: time.Duration(minLatencyMs) * time.Millisecond,
		MaxLatency: time.Duration(maxLatencyMs) * time.Millisecond,
		Now:        time.Now,
	}
}

func (m *MockProvider) Name() string { return "mock" }

// simulatedOutputTokens deterministically derives an output length from
// the input so repeated identical requests produce identical usage
// (useful for tests and for the smoke test's assertions), while still
// varying naturally with prompt length and model "size" the way a real
// completion would: bigger prompts tend to invite longer answers, and we
// cap it so a 50k-token prompt doesn't imply a 50k-token reply.
func simulatedOutputTokens(model string, tokensIn int) int {
	base := 24.0 // minimum reply length, chat models rarely reply in <24 tokens
	scaled := math.Sqrt(float64(tokensIn)) * 8
	mult := 1.0
	switch {
	case strings.Contains(model, "large"):
		mult = 1.6
	case strings.Contains(model, "small"):
		mult = 0.6
	}
	out := int((base + scaled) * mult)
	if out > 4096 {
		out = 4096
	}
	return out
}

func (m *MockProvider) latency() time.Duration {
	spread := int64(m.MaxLatency - m.MinLatency)
	if spread <= 0 {
		return m.MinLatency
	}
	return m.MinLatency + time.Duration(mrand.Int63n(spread))
}

func mockID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "mock-" + hex.EncodeToString(b)
}

func (m *MockProvider) Complete(ctx context.Context, req Request) (Response, error) {
	start := m.Now()
	tokensIn := EstimateMessagesTokens(req.System, req.Messages)
	tokensOut := simulatedOutputTokens(req.Model, tokensIn)
	if req.MaxTokens > 0 && tokensOut > req.MaxTokens {
		tokensOut = req.MaxTokens
	}

	delay := m.latency()
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-time.After(delay):
	}

	content := fmt.Sprintf(
		"[mock:%s] simulated completion for a %d-token prompt (%d simulated output tokens).",
		req.Model, tokensIn, tokensOut,
	)

	usage := Usage{TokensIn: tokensIn, TokensOut: tokensOut}
	return Response{
		ID:           mockID(),
		Model:        req.Model,
		Content:      content,
		FinishReason: "stop",
		Usage:        usage,
		CostUSD:      CostUSD(req.Model, usage.TokensIn, 0, usage.TokensOut),
		LatencyMs:    m.Now().Sub(start).Milliseconds(),
	}, nil
}

func (m *MockProvider) Stream(ctx context.Context, req Request, w StreamWriter) error {
	tokensIn := EstimateMessagesTokens(req.System, req.Messages)
	tokensOut := simulatedOutputTokens(req.Model, tokensIn)
	if req.MaxTokens > 0 && tokensOut > req.MaxTokens {
		tokensOut = req.MaxTokens
	}

	full := fmt.Sprintf(
		"[mock:%s] simulated streamed completion for a %d-token prompt (%d simulated output tokens).",
		req.Model, tokensIn, tokensOut,
	)
	words := strings.Fields(full)

	// Spread the configured latency window across the chunks so a
	// streamed mock call has the same total wall-clock feel as a
	// non-streamed one, chunk-by-chunk (first-byte latency plus
	// per-chunk trickle), rather than all-at-once after one long delay.
	total := m.latency()
	perChunk := total / time.Duration(len(words)+1)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(perChunk):
	}

	for i, word := range words {
		suffix := " "
		if i == len(words)-1 {
			suffix = ""
		}
		if err := w.Write(StreamChunk{DeltaContent: word + suffix}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(perChunk):
		}
	}

	usage := Usage{TokensIn: tokensIn, TokensOut: tokensOut}
	return w.Write(StreamChunk{
		Done:         true,
		FinishReason: "stop",
		Usage:        usage,
	})
}
