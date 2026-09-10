package provider

import (
	"strings"
	"unicode"
)

// EstimateTokens approximates the token count of s using a calibrated
// chars-per-token heuristic rather than a real BPE tokenizer.
//
// Why an approximation and not tiktoken: both OpenAI's o200k_base/
// cl100k_base and Anthropic's tokenizer are byte-pair-encoding vocabularies
// distributed as downloadable rank files (see docs/DECISIONS.md ADR-008).
// Fetching those at container startup would be a network dependency the
// gateway cannot have (this system must run fully offline against the mock
// provider), and vendoring multi-megabyte rank tables per model family is
// unnecessary for what this estimate is used for: a *pre-flight* budget
// check before the real call is made. The authoritative token count for
// billing/metrics always comes from the provider's own response `usage`
// object (or the mock provider's deterministic simulation, which uses this
// same estimator so its numbers are reproducible) and is what actually gets
// reconciled into the Redis bucket after the call completes — see
// internal/budget.Bucket.Reconcile. Being off by +/-15% on the pre-flight
// estimate only affects how conservatively we reserve budget before the
// call, never the final metered amount.
//
// Calibration: empirically, English prose averages ~4 characters per
// token under cl100k_base/o200k_base; code and non-English text skew
// lower (more tokens per character) because of denser punctuation and
// less common subword merges. We approximate this by counting "words"
// (whitespace-delimited runs) and standalone punctuation/symbol runs
// separately, since BPE vocabularies tend to assign short/whole tokens to
// common words and separate tokens to punctuation.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	tokens := 0
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		r := runes[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			start := i
			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i])) {
				i++
			}
			wordLen := i - start
			// ~4 chars/subword-token for alphanumeric runs, minimum 1.
			t := wordLen / 4
			if t < 1 {
				t = 1
			}
			tokens += t
		default:
			// Punctuation/symbols: BPE vocabularies typically spend one
			// token per punctuation rune, occasionally merging repeats
			// (e.g. "..."), so count runs of the *same* rune as ~1 token
			// per 2 repeats.
			start := i
			for i < len(runes) && !unicode.IsSpace(runes[i]) && !unicode.IsLetter(runes[i]) && !unicode.IsDigit(runes[i]) {
				i++
			}
			runLen := i - start
			t := (runLen + 1) / 2
			if t < 1 {
				t = 1
			}
			tokens += t
		}
	}
	return tokens
}

// EstimateMessagesTokens estimates the token cost of a full chat request,
// including the small fixed per-message overhead real chat-format
// tokenizers add for role/name framing (OpenAI's own guidance is ~3-4
// tokens of framing per message plus ~3 for the reply priming; we use the
// same constants Anthropic and OpenAI both document as rules of thumb).
func EstimateMessagesTokens(system string, messages []Message) int {
	total := 0
	if system != "" {
		total += EstimateTokens(system) + 4
	}
	for _, m := range messages {
		total += EstimateTokens(m.Content) + 4
	}
	total += 3 // reply priming
	return total
}

// NormalizeModel strips provider-prefix aliasing (e.g. "openai/gpt-4o" ->
// "gpt-4o") so pricing/lookup keys are consistent regardless of how the
// client specified the model string.
func NormalizeModel(model string) string {
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		return model[idx+1:]
	}
	return model
}
