// Package policy implements the gateway's deterministic decision-maker
// for budget-pressure events. It is the ONLY component that turns an
// advisor suggestion into an accept/reject/partial-allowance outcome --
// see docs/DECISIONS.md ADR-003. Every exported function here is a pure
// function of its inputs: identical (Context, []Suggestion, Config) always
// produces the identical Outcome, with no I/O, no clock reads, and no
// randomness, so its behavior is fully unit-testable and auditable.
package policy

import (
	"github.com/horunu/budget-governor/gateway/internal/budget"
	"github.com/horunu/budget-governor/gateway/internal/provider"
)

// SuggestionType enumerates the advisor suggestion types this policy
// engine knows how to evaluate. A suggestion of any other type is logged
// and ignored -- see docs/DECISIONS.md ADR-003's "cost of this choice."
type SuggestionType string

const (
	SuggestionSwitchModel     SuggestionType = "switch_model"
	SuggestionReduceMaxTokens SuggestionType = "reduce_max_tokens"
	SuggestionSummarizeFirst  SuggestionType = "summarize_first"
	SuggestionReduceToolCalls SuggestionType = "reduce_tool_calls"
)

// Suggestion is the policy engine's view of one ranked suggestion
// returned by the advisor service (see advisor/app/schemas.py for the
// Pydantic-validated shape this is decoded from).
type Suggestion struct {
	Type               SuggestionType
	SuggestedModel     string  // populated for switch_model
	SuggestedMaxTokens int64   // populated for reduce_max_tokens
	Confidence         float64 // 0.0-1.0, as scored by the advisor
	RiskNote           string
}

// Context is everything about the throttled request the policy engine
// needs to evaluate suggestions against -- all of it already known
// deterministically from the failed budget check, never from the LLM.
type Context struct {
	RequestedModel  string
	RequestedTokens int64
	RemainingTokens float64
	RemainingUSD    float64
	FailedScope     budget.Scope
	FailReason      budget.RejectReason
}

// Config tunes the policy engine's acceptance thresholds. Loaded from
// static gateway config, not from the advisor or any per-request input.
type Config struct {
	// MinConfidence is the minimum advisor confidence score required to
	// act on ANY suggestion. Below this, the suggestion is treated as
	// noise regardless of type.
	MinConfidence float64
	// MinPartialAllowanceTokens is the smallest reduce_max_tokens
	// allowance worth granting -- below this a retry is unlikely to
	// produce a useful response, so it's treated as equivalent to reject.
	MinPartialAllowanceTokens int64
}

// DefaultConfig returns the gateway's out-of-the-box policy thresholds.
func DefaultConfig() Config {
	return Config{
		MinConfidence:             0.6,
		MinPartialAllowanceTokens: 64,
	}
}

// Action enumerates what the gateway should actually do as a result of
// a Decide call.
type Action string

const (
	ActionReject           Action = "reject"
	ActionRetryWithModel   Action = "retry_with_model"
	ActionPartialAllowance Action = "partial_allowance"
)

// Outcome is the policy engine's deterministic decision. Advice carries a
// human-readable suggestion surfaced to the client even when Action is
// ActionReject (e.g. a summarize_first suggestion the gateway cannot act
// on server-side, but the caller can on its next request).
type Outcome struct {
	Action    Action
	Model     string // set when Action == ActionRetryWithModel
	MaxTokens int64  // set when Action == ActionPartialAllowance
	Reason    budget.RejectReason
	Advice    string
}

// Decide evaluates ranked suggestions (highest-priority first, as
// returned by the advisor) against ctx and cfg, returning the first
// suggestion that clears the confidence bar AND passes a policy-specific,
// pricing-verified sanity check for its type. A suggestion is never
// trusted at face value -- e.g. a switch_model suggestion's claim that a
// model is "cheaper" is independently verified against the real pricing
// table (internal/provider.CheeperAlternatives), not the LLM's say-so.
//
// If no suggestion is acceptable, Decide falls back to the deterministic
// reject the budget enforcer already computed (ctx.FailReason), optionally
// carrying advisory text from the best-ranked-but-not-actionable
// suggestion so the client still learns something useful from the 429.
func Decide(ctx Context, suggestions []Suggestion, cfg Config) Outcome {
	var advisoryNote string

	for _, s := range suggestions {
		if s.Confidence < cfg.MinConfidence {
			continue
		}

		switch s.Type {
		case SuggestionSwitchModel:
			if isVerifiedCheaperModel(ctx.RequestedModel, s.SuggestedModel) {
				return Outcome{
					Action: ActionRetryWithModel,
					Model:  s.SuggestedModel,
					Advice: s.RiskNote,
				}
			}

		case SuggestionReduceMaxTokens:
			allowance := s.SuggestedMaxTokens
			if allowance > int64(ctx.RemainingTokens) {
				allowance = int64(ctx.RemainingTokens)
			}
			if allowance >= cfg.MinPartialAllowanceTokens && allowance < ctx.RequestedTokens {
				return Outcome{
					Action:    ActionPartialAllowance,
					MaxTokens: allowance,
					Advice:    s.RiskNote,
				}
			}

		case SuggestionSummarizeFirst, SuggestionReduceToolCalls:
			// Not actionable by the gateway on this request (it would
			// require rewriting the client's input, which the gateway
			// does not do), but worth surfacing to the caller for their
			// NEXT request. Only the first such note is kept.
			if advisoryNote == "" {
				advisoryNote = string(s.Type) + ": " + s.RiskNote
			}

		default:
			// Unrecognized suggestion type: ignored by design, see
			// docs/DECISIONS.md ADR-003.
		}
	}

	return Outcome{
		Action: ActionReject,
		Reason: ctx.FailReason,
		Advice: advisoryNote,
	}
}

// isVerifiedCheaperModel independently confirms suggestedModel is
// actually priced lower than requestedModel in the same provider family,
// using the gateway's own pricing table -- never trusting the advisor's
// claim alone.
func isVerifiedCheaperModel(requestedModel, suggestedModel string) bool {
	if suggestedModel == "" || suggestedModel == requestedModel {
		return false
	}
	for _, alt := range provider.CheaperAlternatives(requestedModel) {
		if alt == suggestedModel {
			return true
		}
	}
	return false
}
