// Package auth implements API-key authentication for the gateway hot
// path. It never performs a synchronous Postgres query on a request:
// validated keys are served from an in-memory cache that is refreshed by
// a background goroutine on a fixed interval, with a lazy fallback query
// only on a cold cache miss (first use of a brand new key).
package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Scope is a permission granted to an API key.
type Scope string

const (
	ScopeAdmin  Scope = "admin"
	ScopeAgent  Scope = "agent"
	ScopeViewer Scope = "viewer"
)

// Identity is the resolved principal for an authenticated request.
type Identity struct {
	TenantID string
	KeyID    string
	Scopes   map[Scope]bool
}

func (i Identity) HasScope(s Scope) bool {
	return i.Scopes[s]
}

var (
	ErrKeyNotFound = errors.New("auth: api key not found")
	ErrKeyRevoked  = errors.New("auth: api key revoked")
	ErrKeyInvalid  = errors.New("auth: api key invalid")
)

// keyRecord mirrors the subset of the `api_keys` table the gateway needs.
type keyRecord struct {
	ID        string
	TenantID  string
	KeyPrefix string
	HashedKey string
	Scopes    []string
	RevokedAt sql.NullTime
}

type cacheEntry struct {
	identity  Identity
	expiresAt time.Time
}

// Store resolves API keys to identities, backed by Postgres with an
// in-memory TTL cache in front of it.
type Store struct {
	db     *sql.DB
	ttl    time.Duration
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]cacheEntry // keyed by sha256(raw key) hex, see comment below
}

// NewStore constructs a Store. The background refresh loop is started
// separately via Store.RunRefreshLoop so callers control its lifecycle.
func NewStore(db *sql.DB, ttl time.Duration, logger *slog.Logger) *Store {
	return &Store{
		db:     db,
		ttl:    ttl,
		logger: logger,
		cache:  make(map[string]cacheEntry),
	}
}

// cacheKey hashes the raw presented key with SHA-256 to use as the
// in-memory cache index. This is deliberately NOT the bcrypt hash stored
// in Postgres (bcrypt is salted/slow by design and unsuitable as a map
// key derivation); SHA-256 here only protects against holding raw
// secrets in process memory as plain map keys, it is not the credential
// verification step (bcrypt.CompareHashAndPassword against the stored
// hash is, and only runs on a cache miss).
func cacheKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves a raw API key (as presented in the Authorization
// header) to an Identity. On a cache hit this is a single mutex-guarded
// map read — no I/O. On a miss it falls back to a synchronous Postgres
// lookup + bcrypt verify (acceptable: this only happens once per new key
// per gateway replica per TTL window, not on the steady-state hot path).
func (s *Store) Authenticate(ctx context.Context, rawKey string) (Identity, error) {
	ck := cacheKey(rawKey)

	s.mu.RLock()
	entry, ok := s.cache[ck]
	s.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.identity, nil
	}

	identity, err := s.lookupAndVerify(ctx, rawKey)
	if err != nil {
		return Identity{}, err
	}

	s.mu.Lock()
	s.cache[ck] = cacheEntry{identity: identity, expiresAt: time.Now().Add(s.ttl)}
	s.mu.Unlock()

	return identity, nil
}

const keyPrefixLen = 12

func (s *Store) lookupAndVerify(ctx context.Context, rawKey string) (Identity, error) {
	if len(rawKey) < keyPrefixLen {
		return Identity{}, ErrKeyInvalid
	}
	prefix := rawKey[:keyPrefixLen]

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, key_prefix, hashed_key, scopes, revoked_at
		FROM api_keys
		WHERE key_prefix = $1 AND revoked_at IS NULL
	`, prefix)
	if err != nil {
		return Identity{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var rec keyRecord
		var scopesRaw []byte // JSONB array, e.g. ["admin","agent"]
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.KeyPrefix, &rec.HashedKey, &scopesRaw, &rec.RevokedAt); err != nil {
			return Identity{}, err
		}
		if bcrypt.CompareHashAndPassword([]byte(rec.HashedKey), []byte(rawKey)) == nil {
			scopes, err := parseScopes(scopesRaw)
			if err != nil {
				return Identity{}, err
			}
			return Identity{
				TenantID: rec.TenantID,
				KeyID:    rec.ID,
				Scopes:   scopes,
			}, nil
		}
	}
	return Identity{}, ErrKeyNotFound
}

// parseScopes decodes a JSONB scopes array (e.g. ["admin","agent"]) as
// stored by the control plane -- see migrations/0001_init.sql for why
// this is JSON rather than a native Postgres array (cross-dialect
// portability with the control plane's SQLite-backed test suite).
func parseScopes(raw []byte) (map[Scope]bool, error) {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make(map[Scope]bool, len(list))
	for _, s := range list {
		out[Scope(s)] = true
	}
	return out, nil
}

// RunRefreshLoop periodically drops expired entries so a revoked key
// cannot be served past ttl even without new traffic re-triggering
// expiry checks. It blocks until ctx is cancelled.
func (s *Store) RunRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(s.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evictExpired()
		}
	}
}

func (s *Store) evictExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.cache {
		if now.After(v.expiresAt) {
			delete(s.cache, k)
		}
	}
}

// HashKey bcrypt-hashes a raw API key for storage. Used by the control
// plane's key-issuance endpoint (invoked here too so gateway-side tests
// and fixtures can generate consistent hashes without importing Python).
func HashKey(rawKey string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(rawKey), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}
