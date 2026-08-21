package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AnthropicProvider talks to the Messages API (POST /v1/messages). Shapes
// verified against https://platform.claude.com/docs/en/api (retrieved
// 2026-09-10). anthropic-version is pinned to the stable dated version
// documented at retrieval time.
type AnthropicProvider struct {
	APIKey     string
	BaseURL    string
	APIVersion string
	HTTPClient *http.Client
}

func NewAnthropicProvider(apiKey, baseURL string) *AnthropicProvider {
	return &AnthropicProvider{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIVersion: "2023-06-01",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Stream    bool               `json:"stream,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// anthropicErrorEnvelope matches {"type":"error","error":{"type","message"}}.
type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// defaultMaxTokens is required by the Messages API (max_tokens has no
// server-side default, unlike OpenAI); we fall back to a conservative
// value when the client didn't specify one.
const defaultMaxTokens = 1024

func toAnthropicRequest(req Request) anthropicRequest {
	msgs := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue // system is a top-level field in the Messages API, not a message role
		}
		msgs = append(msgs, anthropicMessage{Role: m.Role, Content: m.Content})
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  msgs,
	}
}

func (p *AnthropicProvider) do(ctx context.Context, body anthropicRequest) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", p.APIVersion)
	return p.HTTPClient.Do(httpReq)
}

func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	body := toAnthropicRequest(req)
	body.Stream = false

	httpResp, err := p.do(ctx, body)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		return Response{}, parseAnthropicError(httpResp)
	}

	var parsed anthropicResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&parsed); err != nil {
		return Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}

	var content strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}

	// Total input tokens = tokens after last cache breakpoint + cache
	// creation + cache read (see docs/DECISIONS.md provider-notes /
	// research appendix: Anthropic's input_tokens alone undercounts).
	totalIn := parsed.Usage.InputTokens + parsed.Usage.CacheCreationInputTokens + parsed.Usage.CacheReadInputTokens
	usage := Usage{TokensIn: totalIn, TokensOut: parsed.Usage.OutputTokens}

	return Response{
		ID:           parsed.ID,
		Model:        parsed.Model,
		Content:      content.String(),
		FinishReason: parsed.StopReason,
		Usage:        usage,
		CostUSD:      CostUSD(req.Model, totalIn, parsed.Usage.CacheReadInputTokens, usage.TokensOut),
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

// anthropicSSEEvent covers the subset of event payloads the gateway needs
// (message_start for initial usage, content_block_delta for text, and
// message_delta for the final output_tokens total).
type anthropicSSEEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *AnthropicProvider) Stream(ctx context.Context, req Request, w StreamWriter) error {
	body := toAnthropicRequest(req)
	body.Stream = true

	httpResp, err := p.do(ctx, body)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		return parseAnthropicError(httpResp)
	}

	var inputTokens, cacheCreation, cacheRead, outputTokens int
	var finishReason string

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var evt anthropicSSEEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "message_start":
			inputTokens = evt.Message.Usage.InputTokens
			cacheCreation = evt.Message.Usage.CacheCreationInputTokens
			cacheRead = evt.Message.Usage.CacheReadInputTokens
		case "content_block_delta":
			if evt.Delta.Type == "text_delta" {
				if err := w.Write(StreamChunk{DeltaContent: evt.Delta.Text}); err != nil {
					return err
				}
			}
		case "message_delta":
			outputTokens = evt.Usage.OutputTokens
			if evt.Delta.StopReason != "" {
				finishReason = evt.Delta.StopReason
			}
		case "message_stop":
			totalIn := inputTokens + cacheCreation + cacheRead
			return w.Write(StreamChunk{
				Done:         true,
				FinishReason: finishReason,
				Usage:        Usage{TokensIn: totalIn, TokensOut: outputTokens},
			})
		case "error":
			return &ProviderError{StatusCode: 0, Type: evt.Error.Type, Message: evt.Error.Message}
		}
	}
	return scanner.Err()
}

func parseAnthropicError(resp *http.Response) error {
	var env anthropicErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	retryAfter := 0
	if ra := resp.Header.Get("retry-after"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			retryAfter = n
		}
	}
	msg := env.Error.Message
	if msg == "" {
		msg = "unknown anthropic error"
	}
	typ := env.Error.Type
	if typ == "" {
		typ = "unknown_error"
	}
	return &ProviderError{
		StatusCode: resp.StatusCode,
		Type:       typ,
		Message:    msg,
		RetryAfter: retryAfter,
	}
}
