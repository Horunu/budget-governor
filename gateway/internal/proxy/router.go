package proxy

import (
	"strings"

	"github.com/horunu/budget-governor/gateway/internal/provider"
)

// ProviderRouter selects which upstream provider.Provider handles a given
// model name. Kept as its own tiny type (rather than a switch inlined in
// the handler) so it's trivial to unit test and to extend with new
// providers without touching request-handling logic.
type ProviderRouter struct {
	OpenAI    provider.Provider
	Anthropic provider.Provider
	Mock      provider.Provider
}

// Select returns the provider.Provider that owns model, based on the
// model name's family prefix -- the same convention used by
// internal/provider/pricing.go's modelFamily.
func (r ProviderRouter) Select(model string) provider.Provider {
	m := provider.NormalizeModel(model)
	switch {
	case strings.HasPrefix(m, "mock-"):
		return r.Mock
	case strings.HasPrefix(m, "claude-"):
		return r.Anthropic
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return r.OpenAI
	default:
		// Unknown model families default to the mock provider rather
		// than erroring, so a demo/test model name always "just works"
		// against the offline path.
		return r.Mock
	}
}
