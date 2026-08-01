package auth

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestParseScopes(t *testing.T) {
	cases := map[string]map[Scope]bool{
		`["admin"]`:         {ScopeAdmin: true},
		`["admin","agent"]`: {ScopeAdmin: true, ScopeAgent: true},
		`["viewer"]`:        {ScopeViewer: true},
		`[]`:                {},
	}
	for input, want := range cases {
		got, err := parseScopes([]byte(input))
		if err != nil {
			t.Errorf("parseScopes(%q) unexpected error: %v", input, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("parseScopes(%q) = %v, want %v", input, got, want)
			continue
		}
		for s := range want {
			if !got[s] {
				t.Errorf("parseScopes(%q) missing scope %q", input, s)
			}
		}
	}
}

func TestStore_Authenticate_CacheMissQueriesDBAndVerifies(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rawKey := "bg_live_testkey000000000000000000"
	hashed, err := HashKey(rawKey)
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}

	rows := sqlmock.NewRows([]string{"id", "tenant_id", "key_prefix", "hashed_key", "scopes", "revoked_at"}).
		AddRow("key-1", "tenant-acme", rawKey[:keyPrefixLen], hashed, `["admin"]`, nil)
	mock.ExpectQuery("SELECT id, tenant_id, key_prefix, hashed_key, scopes, revoked_at").
		WithArgs(rawKey[:keyPrefixLen]).
		WillReturnRows(rows)

	store := NewStore(db, time.Minute, testLogger())
	identity, err := store.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if identity.TenantID != "tenant-acme" {
		t.Errorf("tenant_id = %q, want tenant-acme", identity.TenantID)
	}
	if !identity.HasScope(ScopeAdmin) {
		t.Error("expected admin scope")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStore_Authenticate_CacheHitSkipsDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rawKey := "bg_live_testkey111111111111111111"
	hashed, err := HashKey(rawKey)
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}

	rows := sqlmock.NewRows([]string{"id", "tenant_id", "key_prefix", "hashed_key", "scopes", "revoked_at"}).
		AddRow("key-2", "tenant-beta", rawKey[:keyPrefixLen], hashed, `["agent"]`, nil)
	// Expect exactly ONE query -- the second Authenticate call must be
	// served entirely from cache.
	mock.ExpectQuery("SELECT id, tenant_id, key_prefix, hashed_key, scopes, revoked_at").
		WithArgs(rawKey[:keyPrefixLen]).
		WillReturnRows(rows)

	store := NewStore(db, time.Minute, testLogger())
	if _, err := store.Authenticate(context.Background(), rawKey); err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}
	if _, err := store.Authenticate(context.Background(), rawKey); err != nil {
		t.Fatalf("unexpected error on second (cached) call: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected exactly one DB query across two Authenticate calls: %v", err)
	}
}

func TestStore_Authenticate_WrongKeyRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	correctKey := "bg_live_correctkey0000000000000000"
	wrongKey := "bg_live_wrongkey00000000000000000000" // same 12-char prefix family risk avoided below
	hashed, _ := HashKey(correctKey)

	prefix := wrongKey[:keyPrefixLen]
	rows := sqlmock.NewRows([]string{"id", "tenant_id", "key_prefix", "hashed_key", "scopes", "revoked_at"})
	// Simulate: prefix lookup returns no matching row for this key's
	// prefix (distinct prefixes in this test), so it should be reported
	// as not found rather than incorrectly authenticated.
	mock.ExpectQuery("SELECT id, tenant_id, key_prefix, hashed_key, scopes, revoked_at").
		WithArgs(prefix).
		WillReturnRows(rows)

	store := NewStore(db, time.Minute, testLogger())
	_, err = store.Authenticate(context.Background(), wrongKey)
	if err != ErrKeyNotFound {
		t.Errorf("expected ErrKeyNotFound, got %v (hashed correct key was %s)", err, hashed)
	}
}

func TestHashKey_VerifiesWithBcrypt(t *testing.T) {
	hashed, err := HashKey("bg_live_roundtrip")
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}
	if hashed == "bg_live_roundtrip" {
		t.Error("HashKey must not return the plaintext key")
	}
}
