// Package budget implements the gateway's real-time budget enforcement:
// a three-level (task/agent/tenant) token-and-dollar bucket backed by
// Redis, checked and decremented atomically via a single Lua script per
// request. See checkAndDecrement.lua for the enforcement algorithm and
// docs/DECISIONS.md ADR-002 for why Redis+Lua rather than an in-process
// or Postgres-backed counter.
package budget

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

//go:embed checkAndDecrement.lua
var checkAndDecrementScript string

// Scope identifies which level of the tenant/agent/task hierarchy a
// bucket belongs to.
type Scope string

const (
	ScopeTask   Scope = "task"
	ScopeAgent  Scope = "agent"
	ScopeTenant Scope = "tenant"
)

// RejectReason enumerates why a request was denied, matching the values
// the architecture doc and the gateway's structured logs/metrics use.
type RejectReason string

// Note on over_rate_limit: this system does not implement a separate
// requests-per-second limiter alongside the token bucket -- the bucket's
// refill rate (tokens/sec) already IS the rate limit, since a burst that
// arrives faster than tokens refill is, definitionally, over budget. A
// burst-heavy tenant that wants RPS-style protection independent of
// token volume configures a low-ceiling, fast-refill budget row (e.g. a
// tenant/agent bucket sized to "N small requests per minute" in token
// terms); it still surfaces as one of the three reasons below, because
// mechanically that's exactly what it is.
const (
	ReasonNone             RejectReason = ""
	ReasonOverTaskBudget   RejectReason = "over_task_budget"
	ReasonOverAgentBudget  RejectReason = "over_agent_budget"
	ReasonOverTenantBudget RejectReason = "over_tenant_budget"
	ReasonInvalidRequest   RejectReason = "invalid_request"
)

var scopeReason = map[Scope]RejectReason{
	ScopeTask:   ReasonOverTaskBudget,
	ScopeAgent:  ReasonOverAgentBudget,
	ScopeTenant: ReasonOverTenantBudget,
}

// Limits describes one bucket's static configuration (as cached
// in-memory from Postgres; see ConfigCache). A limit of 0 means that
// dimension is not enforced for this bucket (e.g. a tenant with only a
// dollar cap and no token cap configured).
type Limits struct {
	TokenLimit         int64
	DollarLimit        float64
	RefillTokensPerSec float64
	RefillUSDPerSec    float64
}

// BucketRef pairs a Redis key with its scope and configured limits, in
// most-specific-first order, for one CheckAndDecrement call.
type BucketRef struct {
	Scope  Scope
	Key    string
	Limits Limits
}

// Key builds the canonical Redis key for a bucket. agentID/taskID are
// "-" when not applicable to the scope (e.g. the tenant-scope bucket has
// no agent/task component).
func Key(tenantID, agentID, taskID string, scope Scope) string {
	switch scope {
	case ScopeTask:
		return fmt.Sprintf("budget:%s:%s:%s", tenantID, agentID, taskID)
	case ScopeAgent:
		return fmt.Sprintf("budget:%s:%s:-", tenantID, agentID)
	default:
		return fmt.Sprintf("budget:%s:-:-", tenantID)
	}
}

// Decision is the outcome of a CheckAndDecrement call.
type Decision struct {
	Allowed         bool
	Reason          RejectReason
	FailedScope     Scope
	RemainingTokens float64
	RemainingUSD    float64
}

var ErrBackendUnavailable = errors.New("budget: redis backend unavailable")

// Enforcer evaluates budget buckets against a Redis backend.
type Enforcer struct {
	rdb      *redis.Client
	sha      string
	stateTTL int // seconds; EXPIRE applied to touched bucket keys
}

func NewEnforcer(rdb *redis.Client, stateTTLSeconds int) *Enforcer {
	return &Enforcer{rdb: rdb, stateTTL: stateTTLSeconds}
}

// LoadScript pre-loads the Lua script into Redis's script cache so the
// hot path uses EVALSHA (avoids re-sending script source on every call).
// Call once at startup; safe to call again after a Redis failover since
// SCRIPT LOAD is idempotent for identical source.
func (e *Enforcer) LoadScript(ctx context.Context) error {
	sha, err := e.rdb.ScriptLoad(ctx, checkAndDecrementScript).Result()
	if err != nil {
		return fmt.Errorf("budget: loading lua script: %w", err)
	}
	e.sha = sha
	return nil
}

// CheckAndDecrement evaluates all buckets in refs (most-specific first)
// atomically: either every bucket has sufficient tokens/dollars and all
// are decremented, or none is touched and the first insufficient scope
// is reported.
func (e *Enforcer) CheckAndDecrement(ctx context.Context, refs []BucketRef, requestedTokens int64, requestedUSD float64) (Decision, error) {
	if len(refs) == 0 {
		return Decision{}, errors.New("budget: no bucket refs supplied")
	}

	keys := make([]string, len(refs))
	argv := make([]interface{}, 0, 3+4*len(refs))
	argv = append(argv, requestedTokens, requestedUSD, e.stateTTL)
	for i, r := range refs {
		keys[i] = r.Key
		argv = append(argv, r.Limits.TokenLimit, r.Limits.DollarLimit, r.Limits.RefillTokensPerSec, r.Limits.RefillUSDPerSec)
	}

	res, err := e.eval(ctx, keys, argv)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) != 4 {
		return Decision{}, fmt.Errorf("budget: unexpected script result shape: %#v", res)
	}
	allowed := toInt64(arr[0]) == 1
	failedIdx := toInt64(arr[1])
	remainingTokens := toFloat64(arr[2])
	remainingUSD := toFloat64(arr[3])

	d := Decision{Allowed: allowed, RemainingTokens: remainingTokens, RemainingUSD: remainingUSD}
	if !allowed {
		if failedIdx == -1 {
			d.Reason = ReasonInvalidRequest
		} else if failedIdx >= 1 && int(failedIdx) <= len(refs) {
			scope := refs[failedIdx-1].Scope
			d.FailedScope = scope
			d.Reason = scopeReason[scope]
		}
	}
	return d, nil
}

func (e *Enforcer) eval(ctx context.Context, keys []string, argv []interface{}) (interface{}, error) {
	if e.sha != "" {
		res, err := e.rdb.EvalSha(ctx, e.sha, keys, argv...).Result()
		if err == nil {
			return res, nil
		}
		if !isNoScriptErr(err) {
			return nil, err
		}
		// Redis restarted / flushed its script cache: fall back to EVAL
		// once and re-load for subsequent calls.
	}
	res, err := e.rdb.Eval(ctx, checkAndDecrementScript, keys, argv...).Result()
	if err != nil {
		return nil, err
	}
	_ = e.LoadScript(ctx) // best-effort re-cache; failure here doesn't affect this call's result
	return res, nil
}

func isNoScriptErr(err error) bool {
	return err != nil && len(err.Error()) >= 8 && err.Error()[:8] == "NOSCRIPT"
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	case string:
		var f float64
		fmt.Sscanf(n, "%f", &f)
		return f
	}
	return 0
}

// Reconcile adjusts a bucket after the true token/dollar usage of a
// completed call is known, refunding the difference between the
// pre-flight estimate (already decremented by CheckAndDecrement) and
// actual usage. A negative delta (actual < estimate) increases the
// remaining balance; a positive delta (actual > estimate, e.g. the
// tokenizer under-estimated) decreases it further, potentially past
// zero -- which is fine: it simply makes the next request more likely to
// be throttled, rather than silently under-billing this one.
func (e *Enforcer) Reconcile(ctx context.Context, refs []BucketRef, estimatedTokens int64, estimatedUSD float64, actualTokens int64, actualUSD float64) error {
	deltaTokens := float64(estimatedTokens - actualTokens)
	deltaUSD := estimatedUSD - actualUSD
	if deltaTokens == 0 && deltaUSD == 0 {
		return nil
	}
	pipe := e.rdb.Pipeline()
	for _, r := range refs {
		if r.Limits.TokenLimit > 0 && deltaTokens != 0 {
			pipe.HIncrByFloat(ctx, r.Key, "tokens_remaining", deltaTokens)
		}
		if r.Limits.DollarLimit > 0 && deltaUSD != 0 {
			pipe.HIncrByFloat(ctx, r.Key, "usd_remaining", deltaUSD)
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}
