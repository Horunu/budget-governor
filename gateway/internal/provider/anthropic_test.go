package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicProvider_Complete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Fatalf("unexpected x-api-key %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("unexpected anthropic-version %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture(t, "anthropic_message.json"))
	}))
	defer srv.Close()

	p := NewAnthropicProvider("test-key", srv.URL)
	resp, err := p.Complete(context.Background(), Request{
		Model:     "claude-sonnet-5",
		System:    "You are helpful.",
		Messages:  []Message{{Role: "user", Content: "What is the capital of Germany?"}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Berlin." {
		t.Errorf("content = %q, want %q", resp.Content, "Berlin.")
	}
	// input_tokens(24) + cache_creation(0) + cache_read(8) = 32 total input.
	if resp.Usage.TokensIn != 32 || resp.Usage.TokensOut != 5 {
		t.Errorf("usage = %+v, want tokens_in=32 tokens_out=5", resp.Usage)
	}
	if resp.FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q, want end_turn", resp.FinishReason)
	}
}

func TestAnthropicProvider_Complete_RateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("retry-after", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(fixture(t, "anthropic_error_429.json"))
	}))
	defer srv.Close()

	p := NewAnthropicProvider("test-key", srv.URL)
	_, err := p.Complete(context.Background(), Request{
		Model:     "claude-sonnet-5",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	perr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if perr.StatusCode != 429 || perr.Type != "rate_limit_error" || perr.RetryAfter != 5 {
		t.Errorf("unexpected ProviderError: %+v", perr)
	}
}

func TestAnthropicProvider_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		lines := []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":24,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":8}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Ber"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lin."}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
			`data: {"type":"message_stop"}`,
		}
		for _, l := range lines {
			w.Write([]byte(l + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewAnthropicProvider("test-key", srv.URL)
	var collected []StreamChunk
	err := p.Stream(context.Background(), Request{
		Model:     "claude-sonnet-5",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 100,
	}, streamCollector(&collected))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var full string
	var final StreamChunk
	for _, c := range collected {
		full += c.DeltaContent
		if c.Done {
			final = c
		}
	}
	if full != "Berlin." {
		t.Errorf("streamed content = %q, want %q", full, "Berlin.")
	}
	if final.Usage.TokensIn != 32 || final.Usage.TokensOut != 5 {
		t.Errorf("final usage = %+v, want tokens_in=32 tokens_out=5", final.Usage)
	}
	if final.FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q, want end_turn", final.FinishReason)
	}
}
