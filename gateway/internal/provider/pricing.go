package provider

import "strings"

// PricePerMillion holds per-1M-token USD pricing for one model. CachedInput
// is the price for cache-hit input tokens (0 if the provider doesn't offer
// prompt caching for that model).
type PricePerMillion struct {
	Input       float64
	CachedInput float64
	Output      float64
}

// pricingTable is embedded, current pricing verified 2026-09-10 against:
//   - OpenAI: https://developers.openai.com/api/docs/pricing
//   - Anthropic: https://platform.claude.com/docs/en/about-claude/pricing
//     and https://platform.claude.com/docs/en/api/rate-limits (model roster
//     cross-check)
//
// Prices are USD per 1,000,000 tokens. Keys are the bare model ID (no
// provider prefix) as sent in the request `model` field. See
// docs/DECISIONS.md for how staleness of this table is handled operationally
// (it is not; re-verifying it is a manual, documented process — this is a
// portfolio project, not a billing-critical production system, and a stale
// price here degrades cost estimates, it never bypasses budget enforcement,
// which is denominated in tokens first and dollars only as a convenience
// conversion).
var pricingTable = map[string]PricePerMillion{
	// OpenAI
	"gpt-5.4":      {Input: 2.50, CachedInput: 0.25, Output: 15.00},
	"gpt-5.4-mini": {Input: 0.75, CachedInput: 0.075, Output: 4.50},
	"gpt-5.4-nano": {Input: 0.20, CachedInput: 0.02, Output: 1.25},
	"gpt-5":        {Input: 1.25, CachedInput: 0.125, Output: 10.00},
	"gpt-5-mini":   {Input: 0.25, CachedInput: 0.025, Output: 2.00},
	"gpt-5-nano":   {Input: 0.05, CachedInput: 0.005, Output: 0.40},
	"gpt-4.1":      {Input: 2.00, CachedInput: 0.50, Output: 8.00},
	"gpt-4.1-mini": {Input: 0.40, CachedInput: 0.10, Output: 1.60},
	"gpt-4.1-nano": {Input: 0.10, CachedInput: 0.025, Output: 0.40},
	"gpt-4o":       {Input: 2.50, CachedInput: 1.25, Output: 10.00},
	"gpt-4o-mini":  {Input: 0.15, CachedInput: 0.075, Output: 0.60},
	"o1":           {Input: 15.00, CachedInput: 7.50, Output: 60.00},
	"o3":           {Input: 2.00, CachedInput: 0.50, Output: 8.00},
	"o3-mini":      {Input: 1.10, CachedInput: 0.55, Output: 4.40},
	"o4-mini":      {Input: 1.10, CachedInput: 0.275, Output: 4.40},

	// Anthropic
	"claude-fable-5-1": {Input: 10.00, CachedInput: 0.25, Output: 50.00},
	"claude-opus-5":    {Input: 5.00, CachedInput: 0.50, Output: 25.00},
	"claude-sonnet-5":  {Input: 2.00, CachedInput: 0.20, Output: 10.00},
	"claude-haiku-4-5": {Input: 1.00, CachedInput: 0.10, Output: 5.00},

	// Mock provider models, priced to mirror a cheap/mid/expensive spread
	// so the demo's "switch to a cheaper model" advisor suggestion has a
	// real cost delta to point at.
	"mock-large":  {Input: 5.00, CachedInput: 0.50, Output: 25.00},
	"mock-medium": {Input: 1.00, CachedInput: 0.10, Output: 5.00},
	"mock-small":  {Input: 0.15, CachedInput: 0.015, Output: 0.60},
}

// ErrUnknownModel-backed lookup: CostUSD returns 0 for unknown models
// rather than erroring, since pricing is advisory for budgeting purposes;
// the caller (budget package) treats an unpriced model as dollar-budget-
// exempt and falls back to pure token accounting, which still enforces a
// ceiling.
func Price(model string) (PricePerMillion, bool) {
	p, ok := pricingTable[NormalizeModel(model)]
	return p, ok
}

// CostUSD computes the dollar cost of a completed call. cachedInputTokens
// must be <= tokensIn.
func CostUSD(model string, tokensIn, cachedInputTokens, tokensOut int) float64 {
	p, ok := Price(model)
	if !ok {
		return 0
	}
	uncached := tokensIn - cachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	cost := float64(uncached)/1_000_000*p.Input +
		float64(cachedInputTokens)/1_000_000*p.CachedInput +
		float64(tokensOut)/1_000_000*p.Output
	return cost
}

// CheaperAlternatives returns model IDs priced strictly below `model`'s
// output price, from the same provider family (inferred by name prefix),
// cheapest-appropriate first. Used by the policy engine to evaluate a
// "switch_model" advisor suggestion against real pricing rather than
// trusting the LLM's claim that a model is cheaper.
func CheaperAlternatives(model string) []string {
	base, ok := Price(model)
	if !ok {
		return nil
	}
	family := modelFamily(model)
	var out []string
	for name, p := range pricingTable {
		if name == model {
			continue
		}
		if modelFamily(name) != family {
			continue
		}
		if p.Output < base.Output {
			out = append(out, name)
		}
	}
	return out
}

func modelFamily(model string) string {
	m := NormalizeModel(model)
	switch {
	case strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "openai"
	case strings.HasPrefix(m, "claude-"):
		return "anthropic"
	case strings.HasPrefix(m, "mock-"):
		return "mock"
	default:
		return "unknown"
	}
}
