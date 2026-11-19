package budget

import (
	"database/sql"
	"math"
	"testing"
)

func TestLimitsFromRow_DollarOnlyUsesConfiguredRate(t *testing.T) {
	// $25 cap refilling $25/day = 25/1440 dollars per minute.
	l := limitsFromRow(row{
		DollarLimit:     sql.NullFloat64{Float64: 25, Valid: true},
		RefillPerMinute: sql.NullFloat64{Float64: 25.0 / 1440, Valid: true},
	})
	want := 25.0 / 1440 / 60
	if math.Abs(l.RefillUSDPerSec-want) > 1e-12 {
		t.Errorf("RefillUSDPerSec = %v, want %v ($25/day)", l.RefillUSDPerSec, want)
	}
	if l.TokenLimit != 0 || l.RefillTokensPerSec != 0 {
		t.Errorf("dollar-only row should leave token limits unset, got %+v", l)
	}
}

func TestLimitsFromRow_DollarOnlyNoRefill(t *testing.T) {
	l := limitsFromRow(row{DollarLimit: sql.NullFloat64{Float64: 5, Valid: true}})
	if l.RefillUSDPerSec != 0 {
		t.Errorf("RefillUSDPerSec = %v, want 0 when no refill rate is set", l.RefillUSDPerSec)
	}
}

func TestLimitsFromRow_TokenAndDollarScaleTogether(t *testing.T) {
	l := limitsFromRow(row{
		TokenLimit:      sql.NullInt64{Int64: 1000, Valid: true},
		DollarLimit:     sql.NullFloat64{Float64: 10, Valid: true},
		RefillPerMinute: sql.NullFloat64{Float64: 60, Valid: true}, // 60 tokens/min = 1/s
	})
	if l.RefillTokensPerSec != 1 {
		t.Errorf("RefillTokensPerSec = %v, want 1", l.RefillTokensPerSec)
	}
	if math.Abs(l.RefillUSDPerSec-0.01) > 1e-12 { // 1 token/s * $10/1000 tokens
		t.Errorf("RefillUSDPerSec = %v, want 0.01", l.RefillUSDPerSec)
	}
}
