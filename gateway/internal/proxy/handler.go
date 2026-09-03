package proxy

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/horunu/budget-governor/gateway/internal/auth"
	"github.com/horunu/budget-governor/gateway/internal/budget"
	"github.com/horunu/budget-governor/gateway/internal/observability"
	"github.com/horunu/budget-governor/gateway/internal/policy"
	"github.com/horunu/budget-governor/gateway/internal/provider"
)

// defaultOutputTokenEstimate is used for pre-flight dollar-budget
// estimation when the client didn't specify max_tokens. It intentionally
// over-reserves somewhat (see internal/provider.EstimateTokens docs) --
// the bucket is reconciled to actual usage immediately after the call.
const defaultOutputTokenEstimate = 512

// Handler wires authentication, budget enforcement, provider dispatch,
// the cost advisor, the policy engine, and observability into the
// gateway's single client-facing endpoint. Every field is a narrow
// interface/struct so the handler is unit-testable without a real
// Postgres/Redis (see handler_test.go).
type Handler struct {
	AuthStore    *auth.Store
	ConfigCache  *budget.ConfigCache
	Enforcer     *budget.Enforcer
	Router       ProviderRouter
	Advisor      *AdvisorClient
	PolicyConfig policy.Config
	Metrics      *observability.Metrics
	Logger       *observability.AsyncLogger
	SpendWriter  *SpendWriter
	DB           *sql.DB // used only off the hot path, for recent-call history on budget pressure
	Now          func() time.Time
	Logf         *slog.Logger
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// RegisterRoutes attaches the gateway's HTTP surface to r.
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	r.POST("/v1/chat/completions", h.authMiddleware(auth.ScopeAgent, auth.ScopeAdmin), h.ChatCompletions)
}

// authMiddleware validates the Authorization header and enforces that
// the resolved identity carries at least one of the allowed scopes.
func (h *Handler) authMiddleware(allowed ...auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "missing or malformed Authorization header", "reason": "unauthenticated"}})
			return
		}
		rawKey := strings.TrimPrefix(header, "Bearer ")
		if rawKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "missing or malformed Authorization header", "reason": "unauthenticated"}})
			return
		}

		identity, err := h.AuthStore.Authenticate(c.Request.Context(), rawKey)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid api key", "reason": "unauthenticated"}})
			return
		}

		ok := false
		for _, s := range allowed {
			if identity.HasScope(s) {
				ok = true
				break
			}
		}
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "api key lacks required scope", "reason": "insufficient_scope"}})
			return
		}

		c.Set("identity", identity)
		c.Next()
	}
}

func identityFrom(c *gin.Context) auth.Identity {
	v, _ := c.Get("identity")
	id, _ := v.(auth.Identity)
	return id
}

// ChatCompletions is the gateway's core hot-path handler. See
// docs/ARCHITECTURE.md for the full sequence diagram this implements.
func (h *Handler) ChatCompletions(c *gin.Context) {
	start := h.now()
	traceID := uuid.NewString()
	identity := identityFrom(c)

	var body ClientChatRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{}.withMessage("invalid request body: "+err.Error(), "invalid_request"))
		return
	}

	agentID := c.GetHeader("X-Agent-Id")
	taskID := c.GetHeader("X-Task-Id")

	req := body.toProviderRequest()
	req.TenantID = identity.TenantID
	req.AgentID = agentID
	req.TaskID = taskID
	req.TraceID = traceID

	refs := h.ConfigCache.Resolve(identity.TenantID, agentID, taskID)
	if len(refs) == 0 {
		// Fail closed: a tenant with no budget configured at all cannot
		// spend, by design (see docs/ARCHITECTURE.md's failure-modes
		// table). This is different from a *configured* budget of 0,
		// which would enforce/reject normally via the Lua script.
		h.finish(c, req, nil, budget.Decision{}, "rejected", "no_budget_configured", start, provider.Usage{}, 0, http.StatusForbidden)
		c.JSON(http.StatusForbidden, ErrorResponse{}.withMessage("no budget configured for this tenant/agent/task", "no_budget_configured"))
		return
	}

	estTokensIn := provider.EstimateMessagesTokens(req.System, req.Messages)
	estTokensOut := req.MaxTokens
	if estTokensOut <= 0 {
		estTokensOut = defaultOutputTokenEstimate
	}
	estTokens := int64(estTokensIn + estTokensOut)
	estUSD := provider.CostUSD(req.Model, estTokensIn, 0, estTokensOut)

	decision, err := h.Enforcer.CheckAndDecrement(c.Request.Context(), refs, estTokens, estUSD)
	if err != nil {
		h.Logf.Error("budget backend unavailable", "trace_id", traceID, "error", err)
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{}.withMessage("budget backend unavailable", "budget_backend_unavailable"))
		return
	}

	if decision.Allowed {
		h.dispatch(c, req, refs, estTokens, estUSD, "allowed", "", start)
		return
	}

	// Budget pressure: consult the advisor (bounded, async to the hot
	// path by construction -- this branch is not it), then let the
	// deterministic policy engine decide. See docs/DECISIONS.md ADR-003.
	h.handleBudgetPressure(c, req, refs, decision, estTokensIn, estTokens, estUSD, start)
}

func (h *Handler) handleBudgetPressure(c *gin.Context, req provider.Request, refs []budget.BucketRef, decision budget.Decision, estTokensIn int, estTokens int64, estUSD float64, start time.Time) {
	advisorCtx, cancel := context.WithTimeout(c.Request.Context(), 300*time.Millisecond)
	defer cancel()

	recentCalls := h.recentCalls(advisorCtx, req.TenantID, req.AgentID)

	suggestions, err := h.Advisor.Advise(advisorCtx, adviseRequest{
		TenantID:        req.TenantID,
		AgentID:         req.AgentID,
		TaskID:          req.TaskID,
		RequestedModel:  req.Model,
		EstimatedTokens: estTokens,
		RemainingTokens: decision.RemainingTokens,
		RemainingUSD:    decision.RemainingUSD,
		FailedScope:     string(decision.FailedScope),
		RecentCalls:     recentCalls,
	})
	outcome := "error"
	if err != nil {
		h.Logf.Warn("advisor call failed, falling back to deterministic reject", "trace_id", req.TraceID, "error", err)
		suggestions = nil
	} else if len(suggestions) > 0 {
		outcome = "suggested"
	} else {
		outcome = "no_suggestions"
	}
	h.Metrics.AdvisorInvocations.WithLabelValues(req.TenantID, outcome).Inc()

	policyOutcome := policy.Decide(policy.Context{
		RequestedModel:  req.Model,
		RequestedTokens: estTokens,
		RemainingTokens: decision.RemainingTokens,
		RemainingUSD:    decision.RemainingUSD,
		FailedScope:     decision.FailedScope,
		FailReason:      decision.Reason,
	}, suggestions, h.PolicyConfig)

	switch policyOutcome.Action {
	case policy.ActionRetryWithModel:
		retryReq := req
		retryReq.Model = policyOutcome.Model
		retryTokensOut := retryReq.MaxTokens
		if retryTokensOut <= 0 {
			retryTokensOut = defaultOutputTokenEstimate
		}
		retryTokens := int64(estTokensIn + retryTokensOut)
		retryUSD := provider.CostUSD(retryReq.Model, estTokensIn, 0, retryTokensOut)

		retryDecision, err := h.Enforcer.CheckAndDecrement(c.Request.Context(), refs, retryTokens, retryUSD)
		if err == nil && retryDecision.Allowed {
			h.dispatchAdvised(c, retryReq, refs, retryTokens, retryUSD, policyOutcome.Advice, start)
			return
		}
		h.reject(c, req, decision, policyOutcome, start)

	case policy.ActionPartialAllowance:
		retryReq := req
		retryReq.MaxTokens = int(policyOutcome.MaxTokens)
		retryTokens := int64(estTokensIn) + policyOutcome.MaxTokens
		retryUSD := provider.CostUSD(retryReq.Model, estTokensIn, 0, int(policyOutcome.MaxTokens))

		retryDecision, err := h.Enforcer.CheckAndDecrement(c.Request.Context(), refs, retryTokens, retryUSD)
		if err == nil && retryDecision.Allowed {
			h.dispatchAdvised(c, retryReq, refs, retryTokens, retryUSD, policyOutcome.Advice, start)
			return
		}
		h.reject(c, req, decision, policyOutcome, start)

	default:
		h.reject(c, req, decision, policyOutcome, start)
	}
}

func (h *Handler) reject(c *gin.Context, req provider.Request, decision budget.Decision, outcome policy.Outcome, start time.Time) {
	reason := outcome.Reason
	if reason == "" {
		reason = decision.Reason
	}
	h.finish(c, req, nil, decision, "rejected", string(reason), start, provider.Usage{}, 0, http.StatusTooManyRequests)

	resp := ErrorResponse{}.withMessage("budget exceeded: "+string(reason), string(reason))
	resp.Error.RemainingTokens = decision.RemainingTokens
	resp.Error.RemainingUSD = decision.RemainingUSD
	resp.Error.Advice = outcome.Advice
	c.JSON(http.StatusTooManyRequests, resp)
}

// recentCalls fetches the last 20 spend_events for this agent to give the
// advisor real usage history. Best-effort: a query failure returns an
// empty slice rather than failing the request -- the advisor still
// functions (with less context) if this lookup fails.
func (h *Handler) recentCalls(ctx context.Context, tenantID, agentID string) []CallRecord {
	if h.DB == nil || agentID == "" {
		return nil
	}
	rows, err := h.DB.QueryContext(ctx, `
		SELECT model, tokens_in, tokens_out, cost_usd
		FROM spend_events
		WHERE tenant_id = $1 AND agent_id = $2
		ORDER BY created_at DESC
		LIMIT 20
	`, tenantID, agentID)
	if err != nil {
		h.Logf.Warn("recent call history query failed", "error", err)
		return nil
	}
	defer rows.Close()

	var out []CallRecord
	for rows.Next() {
		var r CallRecord
		if err := rows.Scan(&r.Model, &r.TokensIn, &r.TokensOut, &r.CostUSD); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// dispatch performs the actual upstream call for an allowed request.
func (h *Handler) dispatch(c *gin.Context, req provider.Request, refs []budget.BucketRef, estTokens int64, estUSD float64, decisionLabel, reason string, start time.Time) {
	h.doDispatch(c, req, refs, estTokens, estUSD, decisionLabel, reason, "", start)
}

func (h *Handler) dispatchAdvised(c *gin.Context, req provider.Request, refs []budget.BucketRef, estTokens int64, estUSD float64, advice string, start time.Time) {
	h.doDispatch(c, req, refs, estTokens, estUSD, "advised_retry", "", advice, start)
}

func (h *Handler) doDispatch(c *gin.Context, req provider.Request, refs []budget.BucketRef, estTokens int64, estUSD float64, decisionLabel, reason, advice string, start time.Time) {
	p := h.Router.Select(req.Model)

	if req.Stream {
		h.streamDispatch(c, req, refs, estTokens, estUSD, decisionLabel, advice, start, p)
		return
	}

	resp, err := p.Complete(c.Request.Context(), req)
	if err != nil {
		h.handleProviderError(c, req, refs, estTokens, estUSD, start, err)
		return
	}

	// Reconcile the pre-flight estimate against actual usage -- refund
	// (or, rarely, further debit) the bucket, see budget.Enforcer.Reconcile.
	actualUSD := resp.CostUSD
	if err := h.Enforcer.Reconcile(c.Request.Context(), refs, estTokens, estUSD, int64(resp.Usage.TokensIn+resp.Usage.TokensOut), actualUSD); err != nil {
		h.Logf.Warn("bucket reconcile failed", "trace_id", req.TraceID, "error", err)
	}

	h.finish(c, req, refs, budget.Decision{Allowed: true}, decisionLabel, reason, start, resp.Usage, actualUSD, http.StatusOK)

	out := ClientChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Model:   resp.Model,
		CostUSD: actualUSD,
		Choices: []ClientChatChoice{{
			Index:        0,
			Message:      ClientMessage{Role: "assistant", Content: resp.Content},
			FinishReason: resp.FinishReason,
		}},
		Usage: ClientChatUsage{
			PromptTokens:     resp.Usage.TokensIn,
			CompletionTokens: resp.Usage.TokensOut,
			TotalTokens:      resp.Usage.TokensIn + resp.Usage.TokensOut,
		},
	}
	if advice != "" {
		out.BudgetAdvisory = advice
		c.Header("X-Budget-Advisory", advice)
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) handleProviderError(c *gin.Context, req provider.Request, refs []budget.BucketRef, estTokens int64, estUSD float64, start time.Time, err error) {
	// The call never reached (or never completed enough to bill against)
	// the provider successfully: refund the full reservation.
	if rerr := h.Enforcer.Reconcile(c.Request.Context(), refs, estTokens, estUSD, 0, 0); rerr != nil {
		h.Logf.Warn("bucket refund-on-error failed", "trace_id", req.TraceID, "error", rerr)
	}

	var perr *provider.ProviderError
	status := http.StatusBadGateway
	message := err.Error()
	if errors.As(err, &perr) {
		status = mapProviderStatus(perr.StatusCode)
		message = perr.Message
		h.Metrics.ProviderErrorsTotal.WithLabelValues(providerNameFor(h.Router, req.Model), statusClass(status)).Inc()
	}

	h.finish(c, req, refs, budget.Decision{}, "rejected", "upstream_error", start, provider.Usage{}, 0, status)
	c.JSON(status, ErrorResponse{}.withMessage(message, "upstream_error"))
}

func mapProviderStatus(providerStatus int) int {
	switch {
	case providerStatus == 0:
		return http.StatusBadGateway
	case providerStatus == 429:
		return http.StatusTooManyRequests
	case providerStatus >= 500:
		return http.StatusBadGateway
	default:
		return providerStatus
	}
}

func statusClass(status int) string {
	if status >= 500 {
		return "5xx"
	}
	return "4xx"
}

func providerNameFor(r ProviderRouter, model string) string {
	return r.Select(model).Name()
}

// finish emits the structured log line, Prometheus metrics, and async
// spend-event write common to every terminal outcome (allowed, advised,
// rejected). This is the single place those three observability
// obligations are discharged, so no code path can accidentally skip one.
func (h *Handler) finish(c *gin.Context, req provider.Request, refs []budget.BucketRef, decision budget.Decision, decisionLabel, reason string, start time.Time, usage provider.Usage, costUSD float64, statusCode int) {
	latency := h.now().Sub(start)

	h.Metrics.RequestsTotal.WithLabelValues(req.TenantID, req.Model, decisionLabel).Inc()
	h.Metrics.RequestDuration.WithLabelValues(req.TenantID, req.Model, decisionLabel).Observe(latency.Seconds())
	if usage.TokensIn > 0 {
		h.Metrics.TokensTotal.WithLabelValues(req.TenantID, req.Model, "in").Add(float64(usage.TokensIn))
	}
	if usage.TokensOut > 0 {
		h.Metrics.TokensTotal.WithLabelValues(req.TenantID, req.Model, "out").Add(float64(usage.TokensOut))
	}
	if costUSD > 0 {
		h.Metrics.CostUSDTotal.WithLabelValues(req.TenantID, req.AgentID, req.Model).Add(costUSD)
	}
	if decisionLabel == "rejected" {
		h.Metrics.RejectionsTotal.WithLabelValues(req.TenantID, reason).Inc()
	}
	for _, r := range refs {
		if r.Limits.TokenLimit > 0 {
			saturation := 1 - (decision.RemainingTokens / float64(r.Limits.TokenLimit))
			h.Metrics.BucketSaturation.WithLabelValues(req.TenantID, string(r.Scope)).Set(saturation)
		}
	}

	h.Logger.Log(observability.RequestLog{
		TraceID:        req.TraceID,
		TenantID:       req.TenantID,
		AgentID:        req.AgentID,
		TaskID:         req.TaskID,
		Model:          req.Model,
		Provider:       providerNameFor(h.Router, req.Model),
		TokensIn:       usage.TokensIn,
		TokensOut:      usage.TokensOut,
		CostUSD:        costUSD,
		Decision:       decisionLabel,
		DecisionReason: reason,
		LatencyMs:      latency.Milliseconds(),
		StatusCode:     statusCode,
	})

	if h.SpendWriter != nil {
		h.SpendWriter.Enqueue(SpendEvent{
			TenantID:       req.TenantID,
			AgentID:        req.AgentID,
			TaskID:         req.TaskID,
			Model:          req.Model,
			TokensIn:       usage.TokensIn,
			TokensOut:      usage.TokensOut,
			CostUSD:        costUSD,
			TraceID:        req.TraceID,
			Decision:       decisionLabel,
			DecisionReason: reason,
		})
	}
}

func (e ErrorResponse) withMessage(message, reason string) ErrorResponse {
	e.Error.Message = message
	e.Error.Reason = reason
	return e
}
