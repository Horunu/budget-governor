// Package proxy implements the gateway's client-facing HTTP surface: an
// OpenAI-Chat-Completions-compatible endpoint (the shape most agent
// frameworks and SDKs already speak) that internally normalizes to
// internal/provider.Request, enforces budget via internal/budget, and
// dispatches to whichever upstream (OpenAI, Anthropic, or the Mock
// provider) the requested model belongs to.
package proxy

import "github.com/horunu/budget-governor/gateway/internal/provider"

// ClientMessage mirrors the client-facing (OpenAI-compatible) message shape.
type ClientMessage struct {
	Role    string `json:"role" binding:"required"`
	Content string `json:"content" binding:"required"`
}

// ClientChatRequest is the request body clients send to
// POST /v1/chat/completions. tenant/agent/task scoping comes from the
// API key (tenant) and optional X-Agent-Id / X-Task-Id headers, not the
// body, so the body stays byte-compatible with the real OpenAI API.
type ClientChatRequest struct {
	Model       string          `json:"model" binding:"required"`
	Messages    []ClientMessage `json:"messages" binding:"required,min=1"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

func (r ClientChatRequest) toProviderRequest() provider.Request {
	msgs := make([]provider.Message, 0, len(r.Messages))
	system := ""
	for _, m := range r.Messages {
		if m.Role == "system" && system == "" {
			system = m.Content
			continue
		}
		msgs = append(msgs, provider.Message{Role: m.Role, Content: m.Content})
	}
	return provider.Request{
		Model:       r.Model,
		Messages:    msgs,
		System:      system,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		Stream:      r.Stream,
	}
}

// ClientChatChoice / ClientChatResponse mirror OpenAI's
// chat.completion response shape closely enough for OpenAI-compatible
// client SDKs to parse it unmodified.
type ClientChatChoice struct {
	Index        int           `json:"index"`
	Message      ClientMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type ClientChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ClientChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Model   string             `json:"model"`
	Choices []ClientChatChoice `json:"choices"`
	Usage   ClientChatUsage    `json:"usage"`

	// Budget Governor extensions (additive fields; ignored by standard
	// OpenAI-compatible clients, consumed by ours). Present only when
	// the response involved budget-pressure handling.
	BudgetAdvisory string  `json:"budget_advisory,omitempty"`
	CostUSD        float64 `json:"cost_usd,omitempty"`
}

// ErrorResponse is returned for both budget rejections (429) and
// upstream/provider failures (4xx/5xx), always carrying a machine-
// readable Reason alongside the human Message.
type ErrorResponse struct {
	Error struct {
		Message         string  `json:"message"`
		Reason          string  `json:"reason"`
		RemainingTokens float64 `json:"remaining_tokens,omitempty"`
		RemainingUSD    float64 `json:"remaining_usd,omitempty"`
		Advice          string  `json:"advice,omitempty"`
	} `json:"error"`
}
