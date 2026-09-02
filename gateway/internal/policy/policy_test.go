package policy

import (
	"testing"

	"github.com/horunu/budget-governor/gateway/internal/budget"
)

func baseCtx() Context {
	return Context{
		RequestedModel:  "claude-opus-5",
		RequestedTokens: 500,
		RemainingTokens: 0,
		RemainingUSD:    0,
		FailedScope:     budget.ScopeTenant,
		FailReason:      budget.ReasonOverTenantBudget,
	}
}

// TestDecide_DecisionTable enumerates the policy engine's behavior across
// every suggestion type and edge case. Each case documents what a
// deterministic reviewer should expect -- this table IS the spec.
func TestDecide_DecisionTable(t *testing.T) {
	cfg := DefaultConfig()

	cases := []struct {
		name        string
		ctx         Context
		suggestions []Suggestion
		wantAction  Action
		wantModel   string
		wantMaxTok  int64
		wantReason  budget.RejectReason
	}{
		{
			name:        "no suggestions falls back to deterministic reject",
			ctx:         baseCtx(),
			suggestions: nil,
			wantAction:  ActionReject,
			wantReason:  budget.ReasonOverTenantBudget,
		},
		{
			name: "verified cheaper model is accepted",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-haiku-4-5", Confidence: 0.9},
			},
			wantAction: ActionRetryWithModel,
			wantModel:  "claude-haiku-4-5",
		},
		{
			name: "switch_model to a model that is NOT actually cheaper is rejected",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				// claude-fable-5-1 is priced HIGHER than claude-opus-5 in
				// the pricing table -- the advisor's suggestion is wrong,
				// and the policy engine must not trust it at face value.
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-fable-5-1", Confidence: 0.95},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverTenantBudget,
		},
		{
			name: "switch_model below confidence threshold is ignored",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-haiku-4-5", Confidence: 0.4},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverTenantBudget,
		},
		{
			name: "switch_model to an unpriced/unknown model is rejected",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSwitchModel, SuggestedModel: "not-a-real-model", Confidence: 0.99},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverTenantBudget,
		},
		{
			name: "switch_model to itself is rejected (no-op suggestion)",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-opus-5", Confidence: 0.99},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverTenantBudget,
		},
		{
			name: "reduce_max_tokens within remaining budget is accepted",
			ctx: Context{
				RequestedModel:  "gpt-4o",
				RequestedTokens: 1000,
				RemainingTokens: 200,
				FailedScope:     budget.ScopeAgent,
				FailReason:      budget.ReasonOverAgentBudget,
			},
			suggestions: []Suggestion{
				{Type: SuggestionReduceMaxTokens, SuggestedMaxTokens: 150, Confidence: 0.8},
			},
			wantAction: ActionPartialAllowance,
			wantMaxTok: 150,
		},
		{
			name: "reduce_max_tokens is capped at remaining budget, not the suggestion",
			ctx: Context{
				RequestedModel:  "gpt-4o",
				RequestedTokens: 1000,
				RemainingTokens: 100,
				FailedScope:     budget.ScopeAgent,
				FailReason:      budget.ReasonOverAgentBudget,
			},
			suggestions: []Suggestion{
				// advisor suggests 150, but only 100 remain -- policy
				// engine must cap to what's actually available, never
				// trust the suggested number outright.
				{Type: SuggestionReduceMaxTokens, SuggestedMaxTokens: 150, Confidence: 0.8},
			},
			wantAction: ActionPartialAllowance,
			wantMaxTok: 100,
		},
		{
			name: "reduce_max_tokens below minimum useful allowance is rejected",
			ctx: Context{
				RequestedModel:  "gpt-4o",
				RequestedTokens: 1000,
				RemainingTokens: 10, // < DefaultConfig().MinPartialAllowanceTokens (64)
				FailedScope:     budget.ScopeAgent,
				FailReason:      budget.ReasonOverAgentBudget,
			},
			suggestions: []Suggestion{
				{Type: SuggestionReduceMaxTokens, SuggestedMaxTokens: 10, Confidence: 0.9},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverAgentBudget,
		},
		{
			name: "summarize_first is advisory only, request still rejected",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSummarizeFirst, Confidence: 0.9, RiskNote: "prompt has grown to 12k tokens across the conversation"},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverTenantBudget,
		},
		{
			name: "unrecognized suggestion type is ignored, no panic",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: "made_up_type", Confidence: 0.99},
			},
			wantAction: ActionReject,
			wantReason: budget.ReasonOverTenantBudget,
		},
		{
			name: "first acceptable suggestion in rank order wins over a later, also-acceptable one",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-haiku-4-5", Confidence: 0.8},
				{Type: SuggestionSwitchModel, SuggestedModel: "gpt-5-nano", Confidence: 0.99},
			},
			wantAction: ActionRetryWithModel,
			wantModel:  "claude-haiku-4-5",
		},
		{
			name: "low-confidence suggestion is skipped in favor of a later high-confidence one",
			ctx:  baseCtx(),
			suggestions: []Suggestion{
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-fable-5-1" /* not actually cheaper */, Confidence: 0.99},
				{Type: SuggestionSwitchModel, SuggestedModel: "claude-haiku-4-5", Confidence: 0.7},
			},
			wantAction: ActionRetryWithModel,
			wantModel:  "claude-haiku-4-5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.ctx, tc.suggestions, cfg)
			if got.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q (outcome=%+v)", got.Action, tc.wantAction, got)
			}
			if tc.wantAction == ActionRetryWithModel && got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if tc.wantAction == ActionPartialAllowance && got.MaxTokens != tc.wantMaxTok {
				t.Errorf("MaxTokens = %d, want %d", got.MaxTokens, tc.wantMaxTok)
			}
			if tc.wantAction == ActionReject && got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestDecide_Deterministic verifies repeated calls with identical inputs
// always produce identical outputs -- the core guarantee ADR-003 rests on.
func TestDecide_Deterministic(t *testing.T) {
	ctx := baseCtx()
	suggestions := []Suggestion{
		{Type: SuggestionSwitchModel, SuggestedModel: "claude-haiku-4-5", Confidence: 0.85, RiskNote: "lower reasoning quality"},
	}
	cfg := DefaultConfig()

	first := Decide(ctx, suggestions, cfg)
	for i := 0; i < 100; i++ {
		got := Decide(ctx, suggestions, cfg)
		if got != first {
			t.Fatalf("iteration %d: Decide returned %+v, want identical %+v", i, got, first)
		}
	}
}
