package provider

import (
	"context"
	"testing"
	"time"
)

func TestMockProvider_Complete_Deterministic(t *testing.T) {
	m := NewMockProvider(1, 2) // near-zero latency for fast tests
	req := Request{
		Model:    "mock-medium",
		Messages: []Message{{Role: "user", Content: "Explain distributed rate limiting in one paragraph."}},
	}
	r1, err := m.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r2, err := m.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r1.Usage.TokensIn != r2.Usage.TokensIn || r1.Usage.TokensOut != r2.Usage.TokensOut {
		t.Errorf("expected deterministic usage for identical requests, got %+v vs %+v", r1.Usage, r2.Usage)
	}
	if r1.Usage.TokensIn == 0 {
		t.Error("expected non-zero tokens_in")
	}
	if r1.CostUSD <= 0 {
		t.Error("expected non-zero cost_usd for a priced mock model")
	}
}

func TestMockProvider_LatencyWithinBounds(t *testing.T) {
	m := NewMockProvider(20, 40)
	start := time.Now()
	_, err := m.Complete(context.Background(), Request{
		Model:    "mock-small",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed < 20*time.Millisecond {
		t.Errorf("latency %v below configured min 20ms", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("latency %v suspiciously far above configured max 40ms", elapsed)
	}
}

func TestMockProvider_LargerModelProducesLongerReplies(t *testing.T) {
	m := NewMockProvider(1, 2)
	req := Request{Messages: []Message{{Role: "user", Content: "Summarize the CAP theorem for a junior engineer, with examples."}}}

	reqSmall := req
	reqSmall.Model = "mock-small"
	reqLarge := req
	reqLarge.Model = "mock-large"

	small, err := m.Complete(context.Background(), reqSmall)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	large, err := m.Complete(context.Background(), reqLarge)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if large.Usage.TokensOut <= small.Usage.TokensOut {
		t.Errorf("expected mock-large tokens_out (%d) > mock-small (%d)", large.Usage.TokensOut, small.Usage.TokensOut)
	}
	if large.CostUSD <= small.CostUSD {
		t.Errorf("expected mock-large cost (%v) > mock-small cost (%v)", large.CostUSD, small.CostUSD)
	}
}

func TestMockProvider_RespectsMaxTokens(t *testing.T) {
	m := NewMockProvider(1, 2)
	resp, err := m.Complete(context.Background(), Request{
		Model:     "mock-large",
		Messages:  []Message{{Role: "user", Content: "Write a very long essay about token buckets, distributed systems, and rate limiting theory across many paragraphs."}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Usage.TokensOut != 10 {
		t.Errorf("tokens_out = %d, want capped at 10", resp.Usage.TokensOut)
	}
}

func TestMockProvider_Stream_MatchesCompleteUsage(t *testing.T) {
	m := NewMockProvider(1, 2)
	req := Request{
		Model:    "mock-medium",
		Messages: []Message{{Role: "user", Content: "Explain distributed rate limiting in one paragraph."}},
	}
	nonStream, err := m.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var collected []StreamChunk
	if err := m.Stream(context.Background(), req, streamCollector(&collected)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var final StreamChunk
	var gotContent bool
	for _, c := range collected {
		if c.DeltaContent != "" {
			gotContent = true
		}
		if c.Done {
			final = c
		}
	}
	if !gotContent {
		t.Error("expected at least one non-empty content chunk")
	}
	if final.Usage.TokensIn != nonStream.Usage.TokensIn || final.Usage.TokensOut != nonStream.Usage.TokensOut {
		t.Errorf("stream usage %+v did not match non-stream usage %+v", final.Usage, nonStream.Usage)
	}
}
