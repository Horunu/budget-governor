// Package proxy implements the gateway's client-facing HTTP surface: an
// OpenAI-Chat-Completions-compatible endpoint (the shape most agent
// frameworks and SDKs already speak) that internally normalizes to
// internal/provider.Request, enforces budget via internal/budget, and
// dispatches to whichever upstream (OpenAI, Anthropic, or the Mock
// provider) the requested model belongs to.
package proxy

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/horunu/budget-governor/gateway/internal/provider"
)

// ClientMessage mirrors the client-facing (OpenAI-compatible) message shape.
type ClientMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ClientChatRequest is the request body clients send to
// POST /v1/chat/completions. tenant/agent/task scoping comes from the
// API key (tenant, plus the agent for agent-bound keys) and optional
// X-Agent-Id / X-Task-Id headers, not the body, so the body stays byte-compatible with the real OpenAI API.
type ClientChatRequest struct {
	Model       string          `json:"model" binding:"required"`
	Messages    []ClientMessage `json:"messages" binding:"required,min=1"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

// parseChatRequest decodes an OpenAI Chat Completions body. Every field is
// kept in Request.Raw so tools, tool_calls and content parts reach the
// provider; the text view (Messages/System) only feeds token estimation
// and the mock provider. toolsText is the tool schema JSON, which also
// counts toward input tokens.
func parseChatRequest(body []byte) (req provider.Request, toolsText string, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return req, "", errors.New("body must be a JSON object")
	}
	var view struct {
		Model               string          `json:"model"`
		Messages            []rawMessage    `json:"messages"`
		MaxTokens           int             `json:"max_tokens"`
		MaxCompletionTokens int             `json:"max_completion_tokens"`
		Temperature         float64         `json:"temperature"`
		Stream              bool            `json:"stream"`
		Tools               json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		return req, "", errors.New("invalid field type: " + err.Error())
	}
	if view.Model == "" {
		return req, "", errors.New("model is required")
	}
	if len(view.Messages) == 0 {
		return req, "", errors.New("messages must be a non-empty array")
	}

	req.Raw = raw
	req.Model = view.Model
	req.MaxTokens = view.MaxCompletionTokens
	if req.MaxTokens == 0 {
		req.MaxTokens = view.MaxTokens
	}
	req.Temperature = view.Temperature
	req.Stream = view.Stream
	for _, m := range view.Messages {
		if m.Role == "" {
			return provider.Request{}, "", errors.New("every message needs a role")
		}
		text := m.text()
		if (m.Role == "system" || m.Role == "developer") && req.System == "" {
			req.System = text
			continue
		}
		req.Messages = append(req.Messages, provider.Message{Role: m.Role, Content: text})
	}
	if len(view.Tools) > 0 && string(view.Tools) != "null" {
		toolsText = string(view.Tools)
	}
	return req, toolsText, nil
}

// rawMessage is the subset of an OpenAI message the gateway reads.
type rawMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// text flattens content (a string or an array of parts) plus any tool-call
// arguments into one string for token estimation.
func (m rawMessage) text() string {
	var b strings.Builder
	b.WriteString(provider.ContentText(m.Content))
	for _, tc := range m.ToolCalls {
		b.WriteString(" " + tc.Function.Name + " " + tc.Function.Arguments)
	}
	return b.String()
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
