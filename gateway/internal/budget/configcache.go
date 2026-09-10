package budget

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// row mirrors one `budgets` table row (see migrations/0001_init.sql).
// agent_id/task_id are nullable: NULL means the row is the tenant-level
// (or agent-level) budget rather than a more specific one.
type row struct {
	TenantID        string
	AgentID         sql.NullString
	TaskID          sql.NullString
	TokenLimit      sql.NullInt64
	DollarLimit     sql.NullFloat64
	RefillPerMinute sql.NullFloat64 // refill rate, same unit as TokenLimit/DollarLimit as applicable
}

// configKey identifies one row's scope for map lookup.
type configKey struct {
	TenantID string
	AgentID  string // "" for tenant-level
	TaskID   string // "" for tenant/agent-level
}

// ConfigCache holds budget configuration (limits + refill rates) refreshed
// from Postgres on a fixed interval. The gateway hot path only ever reads
// this in-memory snapshot -- never Postgres directly -- per the ≤10ms p99
// constraint (see docs/ARCHITECTURE.md#hot-path-latency-budget).
type ConfigCache struct {
	db     *sql.DB
	ttl    time.Duration
	logger *slog.Logger

	mu   sync.RWMutex
	data map[configKey]Limits
}

func NewConfigCache(db *sql.DB, ttl time.Duration, logger *slog.Logger) *ConfigCache {
	return &ConfigCache{
		db:     db,
		ttl:    ttl,
		logger: logger,
		data:   make(map[configKey]Limits),
	}
}

// Refresh performs one synchronous reload from Postgres. Called once at
// startup (blocking, so the gateway never serves traffic with an empty
// budget config) and then periodically by RunRefreshLoop in the
// background.
func (c *ConfigCache) Refresh(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
		SELECT tenant_id, agent_id, task_id, token_limit, dollar_limit, refill_rate_per_min
		FROM budgets
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	next := make(map[configKey]Limits)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.TenantID, &r.AgentID, &r.TaskID, &r.TokenLimit, &r.DollarLimit, &r.RefillPerMinute); err != nil {
			return err
		}
		key := configKey{TenantID: r.TenantID}
		if r.AgentID.Valid {
			key.AgentID = r.AgentID.String
		}
		if r.TaskID.Valid {
			key.TaskID = r.TaskID.String
		}
		refillPerSec := 0.0
		if r.RefillPerMinute.Valid {
			refillPerSec = r.RefillPerMinute.Float64 / 60.0
		}
		limits := Limits{}
		if r.TokenLimit.Valid {
			limits.TokenLimit = r.TokenLimit.Int64
			limits.RefillTokensPerSec = refillPerSec
		}
		if r.DollarLimit.Valid {
			limits.DollarLimit = r.DollarLimit.Float64
			// Dollar refill tracks the same per-minute cadence as the
			// token refill, scaled to the dollar ceiling, so a tenant's
			// "burn allowance" replenishes on one consistent schedule
			// regardless of which dimension (tokens vs dollars) is the
			// binding constraint for a given call.
			if r.TokenLimit.Valid && r.TokenLimit.Int64 > 0 {
				limits.RefillUSDPerSec = refillPerSec * (r.DollarLimit.Float64 / float64(r.TokenLimit.Int64))
			} else {
				limits.RefillUSDPerSec = r.DollarLimit.Float64 / 60.0
			}
		}
		next[key] = limits
	}
	if err := rows.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	c.data = next
	c.mu.Unlock()
	return nil
}

// RunRefreshLoop blocks, reloading on c.ttl until ctx is cancelled. Reload
// errors are logged but never dropped-cache: a Postgres hiccup means the
// gateway keeps enforcing against its last-known-good budget config
// rather than failing every request or falling back to unlimited.
func (c *ConfigCache) RunRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Refresh(ctx); err != nil {
				c.logger.Error("budget config refresh failed, keeping stale cache", "error", err)
			}
		}
	}
}

// Seed directly installs a budget config entry without going through
// Postgres -- used by tests (and available for any in-process
// bootstrapping) that want a ConfigCache pre-populated without a real
// database. agentID/taskID "" mean tenant-level/agent-level respectively,
// matching Resolve's key derivation.
func (c *ConfigCache) Seed(tenantID, agentID, taskID string, limits Limits) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[configKey]Limits)
	}
	c.data[configKey{TenantID: tenantID, AgentID: agentID, TaskID: taskID}] = limits
}

// Resolve returns the ordered (most-specific-first) list of BucketRefs
// that apply to a request, based on which scopes have a configured
// budget row. A tenant with no budgets configured at all returns an
// empty slice, which the caller should treat as "no enforcement
// possible" -- see proxy.Handler for how that's surfaced (fail closed,
// not open: an unconfigured tenant cannot make calls, since the entire
// point of this system is that spend requires an explicit budget).
func (c *ConfigCache) Resolve(tenantID, agentID, taskID string) []BucketRef {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var refs []BucketRef
	if taskID != "" {
		if l, ok := c.data[configKey{TenantID: tenantID, AgentID: agentID, TaskID: taskID}]; ok {
			refs = append(refs, BucketRef{Scope: ScopeTask, Key: Key(tenantID, agentID, taskID, ScopeTask), Limits: l})
		}
	}
	if agentID != "" {
		if l, ok := c.data[configKey{TenantID: tenantID, AgentID: agentID}]; ok {
			refs = append(refs, BucketRef{Scope: ScopeAgent, Key: Key(tenantID, agentID, "", ScopeAgent), Limits: l})
		}
	}
	if l, ok := c.data[configKey{TenantID: tenantID}]; ok {
		refs = append(refs, BucketRef{Scope: ScopeTenant, Key: Key(tenantID, "", "", ScopeTenant), Limits: l})
	}
	return refs
}
