package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// agentBody is a typical agent turn: system prompt, a user message with
// content parts, an earlier assistant tool call, two tool results, and the
// tool schema.
const agentBody = `{
  "model": "MODEL",
  "max_tokens": 2000,
  "temperature": 0.2,
  "stop": "END",
  "tool_choice": "required",
  "parallel_tool_calls": false,
  "messages": [
    {"role": "system", "content": "You are a coding agent."},
    {"role": "user", "content": [
      {"type": "text", "text": "Fix the bug in main.go"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
    ]},
    {"role": "assistant", "content": null, "tool_calls": [
      {"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"main.go\"}"}},
      {"id": "call_2", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"go.mod\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_1", "content": "package main"},
    {"role": "tool", "tool_call_id": "call_2", "content": "module x"}
  ],
  "tools": [
    {"type": "function", "function": {"name": "read", "description": "Read a file",
      "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}
  ]
}`

func agentRequest(t *testing.T, model string) Request {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.Replace(agentBody, "MODEL", model, 1)), &raw); err != nil {
		t.Fatal(err)
	}
	return Request{Model: model, MaxTokens: 2000, Raw: raw}
}

func TestToAnthropicAgentRequest_TranslatesToolHistory(t *testing.T) {
	body, err := toAnthropicAgentRequest(agentRequest(t, "claude-sonnet-5"), false)
	if err != nil {
		t.Fatal(err)
	}
	got := string(mustJSON(body))

	for _, want := range []string{
		`"system":"You are a coding agent."`,
		`"max_tokens":2000`,
		`"stop_sequences":["END"]`,
		`"temperature":0.2`,
		`{"source":{"data":"AAAA","media_type":"image/png","type":"base64"},"type":"image"}`,
		`{"id":"call_1","input":{"path":"main.go"},"name":"read","type":"tool_use"}`,
		`"tool_choice":{"disable_parallel_tool_use":true,"type":"any"}`,
		`"input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("translated body missing %s\nbody: %s", want, got)
		}
	}

	msgs := body["messages"].([]anthropicAgentMessage)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (user, assistant, merged tool results)", len(msgs))
	}
	if msgs[2].Role != "user" || len(msgs[2].Content) != 2 || msgs[2].Content[1]["tool_use_id"] != "call_2" {
		t.Errorf("tool results should merge into one user turn, got %+v", msgs[2])
	}
}

func TestOpenAIProvider_Agent_ForwardsBodyAndToolCalls(t *testing.T) {
	var sent map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &sent)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`))
	}))
	defer srv.Close()

	req := agentRequest(t, "gpt-4o")
	req.Model = "gpt-4o-mini" // policy engine swapped the model
	resp, err := NewOpenAIProvider("k", srv.URL).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(sent["model"]) != `"gpt-4o-mini"` {
		t.Errorf("model sent = %s, want the overridden gpt-4o-mini", sent["model"])
	}
	if compact(sent["tools"]) != compact(req.Raw["tools"]) || compact(sent["messages"]) != compact(req.Raw["messages"]) {
		t.Error("tools and messages must reach OpenAI unchanged")
	}
	if !strings.Contains(string(resp.Raw), `"tool_calls":[{"id":"call_9"`) {
		t.Errorf("tool_calls missing from raw response: %s", resp.Raw)
	}
	if resp.Usage.TokensIn != 100 || resp.Usage.TokensOut != 20 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func compact(b []byte) string {
	var out bytes.Buffer
	json.Compact(&out, b)
	return out.String()
}

func TestOpenAIProvider_AgentStream_PassesChunksThrough(t *testing.T) {
	var sent map[string]json.RawMessage
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":1}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &sent)
		for _, c := range chunks {
			w.Write([]byte("data: " + c + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var got []StreamChunk
	if err := NewOpenAIProvider("k", srv.URL).Stream(context.Background(), agentRequest(t, "gpt-4o"), streamCollector(&got)); err != nil {
		t.Fatal(err)
	}
	if string(sent["stream_options"]) != `{"include_usage":true}` {
		t.Errorf("stream_options = %s, want include_usage so spend is recorded", sent["stream_options"])
	}
	if len(got) != len(chunks)+1 {
		t.Fatalf("got %d chunks, want %d raw + 1 done", len(got), len(chunks))
	}
	for i, c := range chunks {
		if string(got[i].Raw) != c {
			t.Errorf("chunk %d altered: %s", i, got[i].Raw)
		}
	}
	if last := got[len(got)-1]; !last.Done || last.Usage.TokensIn != 7 || last.Usage.TokensOut != 3 {
		t.Errorf("final chunk = %+v", last)
	}
}

func TestAnthropicProvider_Agent_ToolUseBecomesToolCalls(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sent)
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"Reading it."},{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"main.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":5}}`))
	}))
	defer srv.Close()

	resp, err := NewAnthropicProvider("k", srv.URL).Complete(context.Background(), agentRequest(t, "claude-sonnet-5"))
	if err != nil {
		t.Fatal(err)
	}
	if sent["tools"] == nil {
		t.Error("tools were not sent to Anthropic")
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Raw, &out); err != nil {
		t.Fatalf("raw response is not OpenAI JSON: %v (%s)", err, resp.Raw)
	}
	c := out.Choices[0]
	if c.FinishReason != "tool_calls" || c.Message.Content != "Reading it." || len(c.Message.ToolCalls) != 1 {
		t.Fatalf("unexpected choice: %+v", c)
	}
	if tc := c.Message.ToolCalls[0]; tc.ID != "toolu_1" || tc.Function.Name != "read" || tc.Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if out.Usage.PromptTokens != 55 {
		t.Errorf("prompt_tokens = %d, want 55 (input + cache read)", out.Usage.PromptTokens)
	}
}

func TestAnthropicProvider_AgentStream_ToolCallDeltas(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":30,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Ok"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`,
		`{"type":"message_stop"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, e := range events {
			w.Write([]byte("event: x\ndata: " + e + "\n\n"))
		}
	}))
	defer srv.Close()

	var got []StreamChunk
	if err := NewAnthropicProvider("k", srv.URL).Stream(context.Background(), agentRequest(t, "claude-sonnet-5"), streamCollector(&got)); err != nil {
		t.Fatal(err)
	}

	var text, args, id, name, finish string
	for _, c := range got[:len(got)-1] {
		var ch struct {
			Choices []struct {
				Delta struct {
					Content   string        `json:"content"`
					ToolCalls []oaiToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(c.Raw, &ch); err != nil {
			t.Fatalf("chunk is not JSON: %s", c.Raw)
		}
		for _, choice := range ch.Choices {
			text += choice.Delta.Content
			for _, tc := range choice.Delta.ToolCalls {
				if *tc.Index != 0 {
					t.Errorf("tool index = %d, want 0 (first tool call)", *tc.Index)
				}
				args += tc.Function.Arguments
				id += tc.ID
				name += tc.Function.Name
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if text != "Ok" || id != "toolu_1" || name != "read" || args != `{"path":"a"}` || finish != "tool_calls" {
		t.Errorf("text=%q id=%q name=%q args=%q finish=%q", text, id, name, args, finish)
	}
	if last := got[len(got)-1]; !last.Done || last.Usage.TokensIn != 30 || last.Usage.TokensOut != 12 {
		t.Errorf("final = %+v", last)
	}
}
