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

// OpenAIProvider talks to the Chat Completions API
// (POST /v1/chat/completions). Shapes verified against
// https://developers.openai.com/api/docs (retrieved 2026-09-10).
type OpenAIProvider struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

func NewOpenAIProvider(apiKey, baseURL string) *OpenAIProvider {
	return &OpenAIProvider{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (p *OpenAIProvider) Name() string { return "openai" }

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type openAIChoice struct {
	Index   int `json:"index"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type openAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

// openAIErrorEnvelope matches OpenAI's {"error": {...}} shape.
type openAIErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}

func toOpenAIRequest(req Request) openAIChatRequest {
	msgs := make([]openAIMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, openAIMessage{Role: m.Role, Content: m.Content})
	}
	var temp *float64
	if req.Temperature != 0 {
		t := req.Temperature
		temp = &t
	}
	return openAIChatRequest{
		Model:               req.Model,
		Messages:            msgs,
		MaxCompletionTokens: req.MaxTokens,
		Temperature:         temp,
		Stream:              req.Stream,
		StreamOptions:       &streamOptions{IncludeUsage: true},
	}
}

func (p *OpenAIProvider) do(ctx context.Context, body openAIChatRequest) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	return p.HTTPClient.Do(httpReq)
}

func (p *OpenAIProvider) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	body := toOpenAIRequest(req)
	body.Stream = false

	httpResp, err := p.do(ctx, body)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		return Response{}, parseOpenAIError(httpResp)
	}

	var parsed openAIChatResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&parsed); err != nil {
		return Response{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("openai: empty choices in response %s", parsed.ID)
	}

	usage := Usage{TokensIn: parsed.Usage.PromptTokens, TokensOut: parsed.Usage.CompletionTokens}
	return Response{
		ID:           parsed.ID,
		Model:        parsed.Model,
		Content:      parsed.Choices[0].Message.Content,
		FinishReason: parsed.Choices[0].FinishReason,
		Usage:        usage,
		// Price against the requested model family (e.g. "gpt-4o-mini"),
		// not the dated snapshot OpenAI echoes back in the response
		// (e.g. "gpt-4o-mini-2024-07-18") — the pricing table is keyed
		// by family, and snapshot pinning doesn't change price.
		CostUSD:   CostUSD(req.Model, usage.TokensIn, parsed.Usage.PromptTokensDetails.CachedTokens, usage.TokensOut),
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// openAIChunk matches a `chat.completion.chunk` SSE data payload.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

func (p *OpenAIProvider) Stream(ctx context.Context, req Request, w StreamWriter) error {
	body := toOpenAIRequest(req)
	body.Stream = true

	httpResp, err := p.do(ctx, body)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		return parseOpenAIError(httpResp)
	}

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return nil
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate keep-alive/comment lines
		}
		if chunk.Usage != nil {
			if err := w.Write(StreamChunk{
				Done: true,
				Usage: Usage{
					TokensIn:  chunk.Usage.PromptTokens,
					TokensOut: chunk.Usage.CompletionTokens,
				},
			}); err != nil {
				return err
			}
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		finish := ""
		if c.FinishReason != nil {
			finish = *c.FinishReason
		}
		if err := w.Write(StreamChunk{DeltaContent: c.Delta.Content, FinishReason: finish}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func parseOpenAIError(resp *http.Response) error {
	var env openAIErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	retryAfter := 0
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			retryAfter = n
		}
	}
	msg := env.Error.Message
	if msg == "" {
		msg = "unknown openai error"
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
