package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"github.com/horunu/budget-governor/gateway/internal/auth"
	"github.com/horunu/budget-governor/gateway/internal/budget"
	"github.com/horunu/budget-governor/gateway/internal/observability"
	"github.com/horunu/budget-governor/gateway/internal/policy"
	"github.com/horunu/budget-governor/gateway/internal/provider"
)

type testEnv struct {
	router      *gin.Engine
	logBuf      *bytes.Buffer
	asyncLogger *observability.AsyncLogger
	metrics     *observability.Metrics
	configCache *budget.ConfigCache
	sqlMock     sqlmock.Sqlmock
}

func setupTestEnv(t *testing.T, advisorURL string) *testEnv {
	t.Helper()
	return setupTestEnvWithOpenAI(t, advisorURL, provider.NewMockProvider(1, 2))
}

// setupTestEnvWithOpenAI is setupTestEnv with gpt-* models routed to openAI.
func setupTestEnvWithOpenAI(t *testing.T, advisorURL string, openAI provider.Provider) *testEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	enforcer := budget.NewEnforcer(rdb, 3600)
	if err := enforcer.LoadScript(context.Background()); err != nil {
		t.Fatalf("load script: %v", err)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	authStore := auth.NewStore(db, time.Minute, logger)

	configCache := budget.NewConfigCache(db, time.Hour, logger)

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	var logBuf bytes.Buffer
	asyncLogger := observability.NewAsyncLoggerWithWriter(1000, &logBuf)

	router := ProviderRouter{
		OpenAI:    openAI,
		Anthropic: provider.NewMockProvider(1, 2),
		Mock:      provider.NewMockProvider(1, 2),
	}

	h := &Handler{
		AuthStore:    authStore,
		ConfigCache:  configCache,
		Enforcer:     enforcer,
		Router:       router,
		Advisor:      NewAdvisorClient(advisorURL, 300*time.Millisecond),
		PolicyConfig: policy.DefaultConfig(),
		Metrics:      metrics,
		Logger:       asyncLogger,
		SpendWriter:  nil, // not under test here; finish() guards nil
		Logf:         logger,
	}

	r := gin.New()
	h.RegisterRoutes(r)

	return &testEnv{router: r, logBuf: &logBuf, asyncLogger: asyncLogger, metrics: metrics, configCache: configCache, sqlMock: mock}
}

// issueKey registers one API key + scopes in the sqlmock DB and returns
// the raw key string clients should send.
func issueKey(t *testing.T, mock sqlmock.Sqlmock, tenantID string, scopes string) string {
	t.Helper()
	return issueAgentKey(t, mock, tenantID, scopes, "")
}

// issueAgentKey is issueKey for a key bound to one agent ("" = tenant-wide).
func issueAgentKey(t *testing.T, mock sqlmock.Sqlmock, tenantID, scopes, agentID string) string {
	t.Helper()
	rawKey := "bg_live_" + tenantID + "_" + scopes
	if len(rawKey) < 12 {
		rawKey = rawKey + "000000000000"
	}
	hashed, err := auth.HashKey(rawKey)
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}
	prefix := rawKey[:12]
	rows := sqlmock.NewRows([]string{"id", "tenant_id", "key_prefix", "hashed_key", "scopes", "revoked_at", "agent_id"}).
		AddRow("key-"+tenantID, tenantID, prefix, hashed, `["`+scopes+`"]`, nil, agentID)
	mock.ExpectQuery("SELECT id, tenant_id, key_prefix, hashed_key, scopes, revoked_at").
		WithArgs(prefix).
		WillReturnRows(rows)
	return rawKey
}

func doRequest(r *gin.Engine, method, path, apiKey string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestChatCompletions_MissingAuth_Returns401(t *testing.T) {
	env := setupTestEnv(t, "")
	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", "", ClientChatRequest{
		Model:    "mock-small",
		Messages: []ClientMessage{{Role: "user", Content: "hi"}},
	}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatCompletions_ViewerScope_Returns403(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueKey(t, env.sqlMock, "tenant-viewer", "viewer")
	env.configCache.Seed("tenant-viewer", "", "", budget.Limits{TokenLimit: 100000})

	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, ClientChatRequest{
		Model:    "mock-small",
		Messages: []ClientMessage{{Role: "user", Content: "hi"}},
	}, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestChatCompletions_NoBudgetConfigured_FailsClosed(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueKey(t, env.sqlMock, "tenant-unconfigured", "agent")

	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, ClientChatRequest{
		Model:    "mock-small",
		Messages: []ClientMessage{{Role: "user", Content: "hi"}},
	}, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (fail closed on unconfigured tenant)", w.Code)
	}
}

func TestChatCompletions_AllowedRequest_EndToEnd(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueKey(t, env.sqlMock, "tenant-acme", "agent")
	env.configCache.Seed("tenant-acme", "", "", budget.Limits{TokenLimit: 100000, RefillTokensPerSec: 0})

	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, ClientChatRequest{
		Model:    "mock-small",
		Messages: []ClientMessage{{Role: "user", Content: "What is the capital of France?"}},
	}, map[string]string{"X-Agent-Id": "agent-1"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp ClientChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		t.Errorf("expected non-empty assistant content, got %+v", resp)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Errorf("expected non-zero usage, got %+v", resp.Usage)
	}

	// Metric emission.
	got := counterVecValue(t, env.metrics.RequestsTotal, "tenant-acme", "mock-small", "allowed")
	if got != 1 {
		t.Errorf("requests_total{allowed} = %v, want 1", got)
	}
	tokensIn := counterVecValue(t, env.metrics.TokensTotal, "tenant-acme", "mock-small", "in")
	if tokensIn == 0 {
		t.Errorf("tokens_total{in} = %v, want > 0", tokensIn)
	}

	// Structured log emission.
	logsFor(t, env, func(entries []observability.RequestLog) {
		if len(entries) != 1 {
			t.Fatalf("expected exactly 1 log entry, got %d", len(entries))
		}
		e := entries[0]
		if e.TenantID != "tenant-acme" || e.AgentID != "agent-1" || e.Decision != "allowed" {
			t.Errorf("unexpected log entry: %+v", e)
		}
		if e.TokensIn == 0 || e.Model != "mock-small" {
			t.Errorf("log entry missing expected fields: %+v", e)
		}
	})
}

func TestChatCompletions_BudgetExhausted_RejectsWithReason(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueKey(t, env.sqlMock, "tenant-broke", "agent")
	// Small enough ceiling that even a short prompt's pre-flight
	// estimate (input tokens + default 512-token output reservation)
	// exceeds it immediately.
	env.configCache.Seed("tenant-broke", "", "", budget.Limits{TokenLimit: 5})

	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, ClientChatRequest{
		Model:    "mock-small",
		Messages: []ClientMessage{{Role: "user", Content: "hi"}},
	}, map[string]string{"X-Agent-Id": "agent-1"})

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s, want 429", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Reason != string(budget.ReasonOverTenantBudget) {
		t.Errorf("reason = %q, want over_tenant_budget", errResp.Error.Reason)
	}

	got := counterVecValue(t, env.metrics.RejectionsTotal, "tenant-broke", string(budget.ReasonOverTenantBudget))
	if got != 1 {
		t.Errorf("rejections_total = %v, want 1", got)
	}
}

func TestChatCompletions_BudgetPressure_AdvisorSwitchesModel(t *testing.T) {
	advisorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"suggestions": []map[string]interface{}{
				{"type": "switch_model", "suggested_model": "mock-small", "confidence": 0.9, "risk_note": "smaller model, may be less thorough"},
			},
		})
	}))
	defer advisorSrv.Close()

	env := setupTestEnv(t, advisorSrv.URL)
	key := issueKey(t, env.sqlMock, "tenant-pressure", "agent")
	// Token ceiling is generous (switching models never changes the
	// token-count estimate), but the DOLLAR ceiling is sized to reject
	// mock-large ($5/$25 per 1M) while comfortably admitting mock-small
	// ($0.15/$0.60 per 1M) at the same token count -- exactly the
	// scenario a switch_model suggestion is supposed to unblock.
	env.configCache.Seed("tenant-pressure", "", "", budget.Limits{TokenLimit: 100000, DollarLimit: 0.0001})

	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, ClientChatRequest{
		Model:     "mock-large",
		Messages:  []ClientMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 20,
	}, map[string]string{"X-Agent-Id": "agent-1"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (advisor should have unblocked via model switch)", w.Code, w.Body.String())
	}
	var resp ClientChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Model != "mock-small" {
		t.Errorf("model = %q, want mock-small (policy engine should have substituted it)", resp.Model)
	}
	if resp.BudgetAdvisory == "" {
		t.Error("expected budget_advisory to be set")
	}
}

func TestChatCompletions_AgentBoundKey_SpendsAsItsAgent(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueAgentKey(t, env.sqlMock, "tenant-team", "agent", "agent-bob")
	env.configCache.Seed("tenant-team", "", "", budget.Limits{TokenLimit: 100000})
	// Bob's own cap is tiny, so if the gateway applies it the call is
	// rejected with an agent-level reason even though the team has room.
	env.configCache.Seed("tenant-team", "agent-bob", "", budget.Limits{TokenLimit: 5})

	msg := ClientChatRequest{Model: "mock-small", Messages: []ClientMessage{{Role: "user", Content: "hi"}}}

	// No header: still capped as Bob, not just the team pool.
	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, msg, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("no header: status = %d, body = %s, want 429 from bob's cap", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Error.Reason != string(budget.ReasonOverAgentBudget) {
		t.Errorf("reason = %q, want %s", errResp.Error.Reason, budget.ReasonOverAgentBudget)
	}

	// Naming a teammate is refused outright (key is cached, no new DB row needed).
	w = doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, msg, map[string]string{"X-Agent-Id": "agent-sarah"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed header: status = %d, want 403", w.Code)
	}
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Error.Reason != "agent_mismatch" {
		t.Errorf("reason = %q, want agent_mismatch", errResp.Error.Reason)
	}
}

func TestChatCompletions_AgentRequest_ToolCallsRoundTrip(t *testing.T) {
	var upstream map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&upstream)
		w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":40,"completion_tokens":9,"total_tokens":49}}`))
	}))
	defer srv.Close()

	env := setupTestEnvWithOpenAI(t, "", provider.NewOpenAIProvider("k", srv.URL))
	key := issueKey(t, env.sqlMock, "tenant-agent", "agent")
	env.configCache.Seed("tenant-agent", "", "", budget.Limits{TokenLimit: 100000})

	// Content parts, a tool result and a tool schema: what OpenCode-style
	// agents send on every turn.
	body := map[string]any{
		"model": "gpt-4o-mini",
		"messages": []any{
			map[string]any{"role": "system", "content": "agent"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "list files"}}},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_0", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_0", "content": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"type": "object"}}}},
	}
	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstream["tools"] == nil || !strings.Contains(string(upstream["messages"]), `"tool_call_id":"call_0"`) {
		t.Errorf("tools/tool messages did not reach the provider: %v", upstream)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		CostUSD float64 `json:"cost_usd"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "tool_calls" || len(resp.Choices[0].Message.ToolCalls) != 1 || resp.Choices[0].Message.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool calls lost on the way back: %s", w.Body.String())
	}
	if resp.CostUSD <= 0 {
		t.Errorf("cost_usd = %v, want the gateway's spend annotation", resp.CostUSD)
	}
}

func TestChatCompletions_ContentPartsAccepted_Mock(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueKey(t, env.sqlMock, "tenant-parts", "agent")
	env.configCache.Seed("tenant-parts", "", "", budget.Limits{TokenLimit: 100000})
	w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, map[string]any{
		"model":    "mock-small",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("content-part array rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestChatCompletions_InvalidBody_Returns400(t *testing.T) {
	env := setupTestEnv(t, "")
	key := issueKey(t, env.sqlMock, "tenant-bad", "agent")
	for name, body := range map[string]any{
		"no model":    map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		"no messages": map[string]any{"model": "mock-small", "messages": []any{}},
		"no role":     map[string]any{"model": "mock-small", "messages": []any{map[string]any{"content": "hi"}}},
	} {
		w := doRequest(env.router, http.MethodPost, "/v1/chat/completions", key, body, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}
}

func counterVecValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func logsFor(t *testing.T, env *testEnv, fn func([]observability.RequestLog)) {
	t.Helper()
	env.asyncLogger.Drain()
	var entries []observability.RequestLog
	dec := json.NewDecoder(bytes.NewReader(env.logBuf.Bytes()))
	for dec.More() {
		var e observability.RequestLog
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decode log entry: %v", err)
		}
		entries = append(entries, e)
	}
	fn(entries)
}
