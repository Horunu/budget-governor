package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/horunu/budget-governor/gateway/internal/policy"
)

// AdvisorClient calls the cost advisor service. It is invoked ONLY from
// the budget-pressure branch of the request handler -- never on the
// steady-state hot path -- and is bounded by a short timeout so its
// absence or slowness cannot indefinitely stall the one throttled
// request it's consulted for. See docs/DECISIONS.md ADR-003.
type AdvisorClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewAdvisorClient(baseURL string, timeout time.Duration) *AdvisorClient {
	return &AdvisorClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// CallRecord summarizes one recent LLM call for this agent, giving the
// advisor's LLM real usage history to reason about instead of only the
// single throttled request.
type CallRecord struct {
	Model     string  `json:"model"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

type adviseRequest struct {
	TenantID        string       `json:"tenant_id"`
	AgentID         string       `json:"agent_id"`
	TaskID          string       `json:"task_id"`
	RequestedModel  string       `json:"requested_model"`
	EstimatedTokens int64        `json:"estimated_tokens"`
	RemainingTokens float64      `json:"remaining_tokens"`
	RemainingUSD    float64      `json:"remaining_usd"`
	FailedScope     string       `json:"failed_scope"`
	RecentCalls     []CallRecord `json:"recent_calls"`
}

type adviseSuggestion struct {
	Type               string  `json:"type"`
	SuggestedModel     string  `json:"suggested_model,omitempty"`
	SuggestedMaxTokens int64   `json:"suggested_max_tokens,omitempty"`
	Confidence         float64 `json:"confidence"`
	RiskNote           string  `json:"risk_note,omitempty"`
}

type adviseResponse struct {
	Suggestions []adviseSuggestion `json:"suggestions"`
}

// Advise asks the advisor service for ranked cost-reduction suggestions.
// A network error, non-200, or malformed body returns (nil, err) rather
// than panicking; the caller (proxy.Handler) treats any error the same
// way -- as "no suggestions" -- and falls through to the policy engine's
// deterministic reject.
func (c *AdvisorClient) Advise(ctx context.Context, req adviseRequest) ([]policy.Suggestion, error) {
	buf, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/advise", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("advisor: unexpected status %d", resp.StatusCode)
	}

	var parsed adviseResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("advisor: decode response: %w", err)
	}

	out := make([]policy.Suggestion, 0, len(parsed.Suggestions))
	for _, s := range parsed.Suggestions {
		out = append(out, policy.Suggestion{
			Type:               policy.SuggestionType(s.Type),
			SuggestedModel:     s.SuggestedModel,
			SuggestedMaxTokens: s.SuggestedMaxTokens,
			Confidence:         s.Confidence,
			RiskNote:           s.RiskNote,
		})
	}
	return out, nil
}
