package provider

// Agent-compatible request/response handling. Agent clients (OpenCode,
// the Vercel AI SDK, LangChain, aider, ...) speak OpenAI Chat Completions
// with tools, tool_calls, tool results and content-part arrays. When a
// Request carries Raw, the OpenAI provider forwards that body as-is and the
// Anthropic provider translates it in full, in both directions, so the
// agent's tool loop works through the gateway.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// agentDefaultMaxTokens is the Anthropic max_tokens used when an agent
// request names none. Agents write whole files in one tool call, so the
// 1024 default of the text-only path would truncate them mid-call.
const agentDefaultMaxTokens = 8192

// ContentText flattens an OpenAI message content (a string, an array of
// parts, or null) into its text.
func ContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ---- OpenAI: forward the client body ----

// openAIRawBody is the client's body with the gateway's overrides: model
// and max tokens (the policy engine may change both) and stream options
// (usage on the last chunk is how streamed spend gets recorded).
func openAIRawBody(req Request, stream bool) map[string]json.RawMessage {
	body := make(map[string]json.RawMessage, len(req.Raw)+2)
	for k, v := range req.Raw {
		body[k] = v
	}
	body["model"] = mustJSON(req.Model)
	if req.MaxTokens > 0 {
		if _, ok := body["max_tokens"]; ok {
			body["max_tokens"] = mustJSON(req.MaxTokens)
		} else {
			body["max_completion_tokens"] = mustJSON(req.MaxTokens)
		}
	}
	body["stream"] = mustJSON(stream)
	if stream {
		body["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		delete(body, "stream_options")
	}
	return body
}

// ---- Anthropic: translate requests ----

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type block = map[string]any

type anthropicAgentMessage struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// toAnthropicAgentRequest translates an OpenAI Chat Completions body into a
// Messages API body, keeping tools and the tool-call/tool-result history.
func toAnthropicAgentRequest(req Request, stream bool) (map[string]any, error) {
	var msgs []oaiMessage
	if err := json.Unmarshal(req.Raw["messages"], &msgs); err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}

	var system []string
	var out []anthropicAgentMessage
	add := func(role string, blocks []block) {
		if len(blocks) == 0 {
			return
		}
		// The Messages API wants alternating roles: merge runs (e.g. several
		// tool results in a row become one user turn).
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropicAgentMessage{Role: role, Content: blocks})
	}

	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if t := ContentText(m.Content); t != "" {
				system = append(system, t)
			}
		case "tool":
			add("user", []block{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": ContentText(m.Content)}})
		case "assistant":
			blocks := contentBlocks(m.Content)
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if strings.TrimSpace(tc.Function.Arguments) != "" {
					var parsed map[string]any
					if json.Unmarshal([]byte(tc.Function.Arguments), &parsed) == nil {
						input = parsed
					}
				}
				blocks = append(blocks, block{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
			}
			add("assistant", blocks)
		default: // user
			add("user", contentBlocks(m.Content))
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = agentDefaultMaxTokens
	}
	body := map[string]any{"model": req.Model, "max_tokens": maxTokens, "messages": out}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n\n")
	}
	if stream {
		body["stream"] = true
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req.Raw[k]; ok && string(v) != "null" {
			body[k] = v
		}
	}
	if v, ok := req.Raw["stop"]; ok && string(v) != "null" {
		var one string
		var many []string
		if json.Unmarshal(v, &one) == nil && one != "" {
			body["stop_sequences"] = []string{one}
		} else if json.Unmarshal(v, &many) == nil && len(many) > 0 {
			body["stop_sequences"] = many
		}
	}

	if v, ok := req.Raw["tools"]; ok && string(v) != "null" {
		var tools []oaiTool
		if err := json.Unmarshal(v, &tools); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
		var aTools []block
		for _, t := range tools {
			if t.Type != "" && t.Type != "function" {
				continue
			}
			schema := t.Function.Parameters
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tool := block{"name": t.Function.Name, "input_schema": schema}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			aTools = append(aTools, tool)
		}
		if len(aTools) > 0 {
			body["tools"] = aTools
			if choice := anthropicToolChoice(req.Raw); choice != nil {
				body["tool_choice"] = choice
			}
		}
	}
	return body, nil
}

// contentBlocks turns OpenAI content (string or parts) into Messages API
// blocks. Empty text is dropped: the API rejects empty text blocks.
func contentBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []block{{"type": "text", "text": s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []block
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, block{"type": "text", "text": p.Text})
			}
		case "image_url":
			if src := imageSource(p.ImageURL.URL); src != nil {
				out = append(out, block{"type": "image", "source": src})
			}
		}
	}
	return out
}

// imageSource maps an OpenAI image URL (https or a base64 data URL) to a
// Messages API image source.
func imageSource(url string) block {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
		if !found || !isBase64 {
			return nil
		}
		return block{"type": "base64", "media_type": mediaType, "data": data}
	}
	if url == "" {
		return nil
	}
	return block{"type": "url", "url": url}
}

func anthropicToolChoice(raw map[string]json.RawMessage) block {
	var choice block
	if v, ok := raw["tool_choice"]; ok {
		var s string
		var obj struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		switch {
		case json.Unmarshal(v, &s) == nil:
			switch s {
			case "required":
				choice = block{"type": "any"}
			case "none":
				choice = block{"type": "none"}
			default:
				choice = block{"type": "auto"}
			}
		case json.Unmarshal(v, &obj) == nil && obj.Function.Name != "":
			choice = block{"type": "tool", "name": obj.Function.Name}
		}
	}
	if v, ok := raw["parallel_tool_calls"]; ok && string(v) == "false" {
		if choice == nil {
			choice = block{"type": "auto"}
		}
		if choice["type"] != "none" {
			choice["disable_parallel_tool_use"] = true
		}
	}
	return choice
}

// ---- Anthropic: translate responses ----

func openAIFinishReason(stopReason string) string {
	switch stopReason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default: // end_turn, stop_sequence, pause_turn, refusal
		return "stop"
	}
}

type oaiToolCall struct {
	Index    *int   `json:"index,omitempty"` // set only in stream deltas
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// anthropicToOpenAICompletion renders a Messages API response as an OpenAI
// chat.completion, with tool_use blocks as tool_calls.
func anthropicToOpenAICompletion(parsed anthropicResponse, usage Usage) json.RawMessage {
	var text strings.Builder
	var calls []oaiToolCall
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			tc := oaiToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			tc.Function.Arguments = string(b.Input)
			if tc.Function.Arguments == "" {
				tc.Function.Arguments = "{}"
			}
			calls = append(calls, tc)
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return mustJSON(map[string]any{
		"id":      parsed.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   parsed.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": openAIFinishReason(parsed.StopReason)}},
		"usage":   map[string]int{"prompt_tokens": usage.TokensIn, "completion_tokens": usage.TokensOut, "total_tokens": usage.TokensIn + usage.TokensOut},
	})
}

// anthropicChunker turns Messages API stream events into OpenAI
// chat.completion.chunk payloads.
type anthropicChunker struct {
	id, model string
	created   int64
	toolIndex map[int]int // Anthropic content-block index -> OpenAI tool_calls index
}

func (a *anthropicChunker) chunk(delta map[string]any, finish *string) []byte {
	return mustJSON(map[string]any{
		"id": a.id, "object": "chat.completion.chunk", "created": a.created, "model": a.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}

func (a *anthropicChunker) toolDelta(blockIndex int, tc oaiToolCall) []byte {
	i, ok := a.toolIndex[blockIndex]
	if !ok {
		i = len(a.toolIndex)
		a.toolIndex[blockIndex] = i
	}
	tc.Index = &i
	return a.chunk(map[string]any{"tool_calls": []oaiToolCall{tc}}, nil)
}

func (a *anthropicChunker) usageChunk(u Usage) []byte {
	return mustJSON(map[string]any{
		"id": a.id, "object": "chat.completion.chunk", "created": a.created, "model": a.model,
		"choices": []any{},
		"usage":   map[string]int{"prompt_tokens": u.TokensIn, "completion_tokens": u.TokensOut, "total_tokens": u.TokensIn + u.TokensOut},
	})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only called on values built here, which always marshal
	}
	return b
}
