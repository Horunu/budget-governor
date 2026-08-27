package budget

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestEnforcer(t *testing.T) (*Enforcer, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	e := NewEnforcer(rdb, 3600)
	if err := e.LoadScript(context.Background()); err != nil {
		t.Fatalf("load script: %v", err)
	}
	return e, mr
}

func tenantOnlyRefs(tenantID string, tokenLimit int64, refillPerSec float64) []BucketRef {
	return []BucketRef{
		{
			Scope: ScopeTenant,
			Key:   Key(tenantID, "-", "-", ScopeTenant),
			Limits: Limits{
				TokenLimit:         tokenLimit,
				RefillTokensPerSec: refillPerSec,
			},
		},
	}
}

func TestCheckAndDecrement_AllowsWithinBudget(t *testing.T) {
	e, _ := newTestEnforcer(t)
	refs := tenantOnlyRefs("acme", 1000, 0)

	d, err := e.CheckAndDecrement(context.Background(), refs, 100, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("expected allowed, got rejected: %+v", d)
	}
	if d.RemainingTokens != 900 {
		t.Errorf("remaining = %v, want 900", d.RemainingTokens)
	}
}

func TestCheckAndDecrement_RejectsOverBudget(t *testing.T) {
	e, _ := newTestEnforcer(t)
	refs := tenantOnlyRefs("acme", 100, 0)

	d, err := e.CheckAndDecrement(context.Background(), refs, 500, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed {
		t.Fatal("expected rejection, got allowed")
	}
	if d.Reason != ReasonOverTenantBudget {
		t.Errorf("reason = %q, want over_tenant_budget", d.Reason)
	}
	if d.RemainingTokens != 100 {
		t.Errorf("remaining should be untouched at 100, got %v", d.RemainingTokens)
	}

	// Rejected request must not have decremented anything.
	d2, err := e.CheckAndDecrement(context.Background(), refs, 100, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d2.Allowed || d2.RemainingTokens != 0 {
		t.Errorf("expected full 100 tokens still available, got %+v", d2)
	}
}

func TestCheckAndDecrement_ZeroBudget(t *testing.T) {
	e, _ := newTestEnforcer(t)
	refs := tenantOnlyRefs("broke-tenant", 0, 0) // TokenLimit=0 means "not enforced"

	// A zero *limit* means the dimension isn't enforced -- verify a
	// tenant with an explicit, enforced, exhausted budget (limit=10,
	// already spent) rejects correctly instead.
	exhausted := []BucketRef{
		{
			Scope:  ScopeTenant,
			Key:    Key("broke-tenant", "-", "-", ScopeTenant),
			Limits: Limits{TokenLimit: 10},
		},
	}
	d, err := e.CheckAndDecrement(context.Background(), exhausted, 10, 0)
	if err != nil || !d.Allowed {
		t.Fatalf("expected initial spend to succeed: allowed=%v err=%v", d.Allowed, err)
	}
	d2, err := e.CheckAndDecrement(context.Background(), exhausted, 1, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d2.Allowed {
		t.Fatal("expected rejection once budget is fully exhausted")
	}
	if d2.RemainingTokens != 0 {
		t.Errorf("remaining = %v, want 0", d2.RemainingTokens)
	}

	// refs with TokenLimit=0 (unenforced) should always allow, regardless
	// of how many tokens are requested.
	d3, err := e.CheckAndDecrement(context.Background(), refs, 1_000_000, 0)
	if err != nil || !d3.Allowed {
		t.Fatalf("expected unenforced dimension to always allow: %+v err=%v", d3, err)
	}
}

func TestCheckAndDecrement_Refill(t *testing.T) {
	e, mr := newTestEnforcer(t)
	// 10 tokens/sec refill, 100 token ceiling.
	refs := tenantOnlyRefs("acme", 100, 10)

	// The Lua script sources "now" from Redis's own TIME command (not
	// the client's clock, see checkAndDecrement.lua's clock-skew
	// rationale), so the test controls time via miniredis's SetTime
	// rather than FastForward (which only decays key TTLs, not TIME).
	t0 := time.Now()
	mr.SetTime(t0)

	d, err := e.CheckAndDecrement(context.Background(), refs, 100, 0)
	if err != nil || !d.Allowed {
		t.Fatalf("expected initial spend to succeed: %+v err=%v", d, err)
	}
	d2, _ := e.CheckAndDecrement(context.Background(), refs, 1, 0)
	if d2.Allowed {
		t.Fatal("expected rejection immediately after exhausting budget")
	}

	// Advance Redis's clock by 5 seconds -> 50 tokens should have refilled.
	mr.SetTime(t0.Add(5 * time.Second))

	d3, err := e.CheckAndDecrement(context.Background(), refs, 40, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d3.Allowed {
		t.Fatalf("expected refill to allow a 40-token request after 5s at 10/s: %+v", d3)
	}
	if d3.RemainingTokens != 10 {
		t.Errorf("remaining = %v, want 10 (50 refilled - 40 spent)", d3.RemainingTokens)
	}

	// Refill must not exceed the configured ceiling even after a long
	// idle period.
	mr.SetTime(t0.Add(1 * time.Hour))
	d4, err := e.CheckAndDecrement(context.Background(), refs, 100, 0)
	if err != nil || !d4.Allowed {
		t.Fatalf("expected full refill up to ceiling: %+v err=%v", d4, err)
	}
	if d4.RemainingTokens != 0 {
		t.Errorf("remaining = %v, want 0 (100 ceiling - 100 spent)", d4.RemainingTokens)
	}
}

func TestCheckAndDecrement_NestedScopesTaskFailsBeforeTenant(t *testing.T) {
	e, _ := newTestEnforcer(t)
	refs := []BucketRef{
		{Scope: ScopeTask, Key: Key("acme", "agent-1", "task-1", ScopeTask), Limits: Limits{TokenLimit: 5}},
		{Scope: ScopeAgent, Key: Key("acme", "agent-1", "", ScopeAgent), Limits: Limits{TokenLimit: 1000}},
		{Scope: ScopeTenant, Key: Key("acme", "", "", ScopeTenant), Limits: Limits{TokenLimit: 1000}},
	}

	d, err := e.CheckAndDecrement(context.Background(), refs, 50, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed {
		t.Fatal("expected rejection: task budget (5) < requested (50)")
	}
	if d.Reason != ReasonOverTaskBudget {
		t.Errorf("reason = %q, want over_task_budget", d.Reason)
	}

	// Confirm the agent/tenant buckets were NOT decremented despite
	// being individually sufficient -- atomicity across nested scopes.
	agentTenantRefs := refs[1:]
	d2, err := e.CheckAndDecrement(context.Background(), agentTenantRefs, 1000, 0)
	if err != nil || !d2.Allowed {
		t.Fatalf("expected agent+tenant buckets untouched and able to absorb full 1000: %+v err=%v", d2, err)
	}
}

func TestCheckAndDecrement_ConcurrentRequestsNeverOverspend(t *testing.T) {
	e, _ := newTestEnforcer(t)
	refs := tenantOnlyRefs("acme", 1000, 0)

	const workers = 50
	const perWorker = 30 // 50*30 = 1500 requested against a 1000 budget
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := e.CheckAndDecrement(context.Background(), refs, perWorker, 0)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if d.Allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Exactly floor(1000/30) = 33 requests should have been admitted;
	// any more would mean the bucket over-spent under concurrency.
	want := 1000 / perWorker
	if allowedCount != want {
		t.Errorf("allowedCount = %d, want exactly %d (over- or under-admission indicates a race)", allowedCount, want)
	}
}

func TestReconcile_RefundsOverEstimate(t *testing.T) {
	e, _ := newTestEnforcer(t)
	refs := tenantOnlyRefs("acme", 1000, 0)

	d, err := e.CheckAndDecrement(context.Background(), refs, 200, 0) // estimate: 200
	if err != nil || !d.Allowed {
		t.Fatalf("expected allowed: %+v err=%v", d, err)
	}
	if d.RemainingTokens != 800 {
		t.Fatalf("remaining after estimate = %v, want 800", d.RemainingTokens)
	}

	// Actual usage came in lower than estimated (120 vs 200) -- refund 80.
	if err := e.Reconcile(context.Background(), refs, 200, 0, 120, 0); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	d2, err := e.CheckAndDecrement(context.Background(), refs, 880, 0)
	if err != nil || !d2.Allowed {
		t.Fatalf("expected 880 available after refund (800+80): %+v err=%v", d2, err)
	}
	if d2.RemainingTokens != 0 {
		t.Errorf("remaining = %v, want 0", d2.RemainingTokens)
	}
}
