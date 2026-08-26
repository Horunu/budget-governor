package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestOpenAIProvider_Complete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected auth header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture(t, "openai_chat_completion.json"))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	resp, err := p.Complete(context.Background(), Request{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "What is the capital of Germany?"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Berlin." {
		t.Errorf("content = %q, want %q", resp.Content, "Berlin.")
	}
	if resp.Usage.TokensIn != 42 || resp.Usage.TokensOut != 5 {
		t.Errorf("usage = %+v, want tokens_in=42 tokens_out=5", resp.Usage)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.FinishReason)
	}
	// gpt-4o-mini: $0.15/$0.60 per 1M, with 10 cached tokens at $0.075/1M.
	wantCost := float64(32)/1_000_000*0.15 + float64(10)/1_000_000*0.075 + float64(5)/1_000_000*0.60
	if diff := resp.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost_usd = %v, want %v", resp.CostUSD, wantCost)
	}
}

func TestOpenAIProvider_Complete_RateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "13")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(fixture(t, "openai_error_429.json"))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	_, err := p.Complete(context.Background(), Request{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	perr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if perr.StatusCode != 429 {
		t.Errorf("status = %d, want 429", perr.StatusCode)
	}
	if perr.Type != "rate_limit_error" {
		t.Errorf("type = %q, want rate_limit_error", perr.Type)
	}
	if perr.RetryAfter != 13 {
		t.Errorf("retry_after = %d, want 13", perr.RetryAfter)
	}
}

func TestOpenAIProvider_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"Ber"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"lin."},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":5,"total_tokens":47}}`,
			`data: [DONE]`,
		}
		for _, l := range lines {
			w.Write([]byte(l + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	var collected []StreamChunk
	err := p.Stream(context.Background(), Request{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   true,
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
	if final.Usage.TokensIn != 42 || final.Usage.TokensOut != 5 {
		t.Errorf("final usage = %+v", final.Usage)
	}
}

type collectorWriter struct {
	chunks *[]StreamChunk
}

func (c collectorWriter) Write(chunk StreamChunk) error {
	*c.chunks = append(*c.chunks, chunk)
	return nil
}

func streamCollector(out *[]StreamChunk) StreamWriter {
	return collectorWriter{chunks: out}
}
