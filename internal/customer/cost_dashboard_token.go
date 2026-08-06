package customer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// costDashboardTokenPrefix — visible in logs, distinguishes the public
// embeddable cost-dashboard credential from the magic-link
// (vlx_cpml_), portal session (vlx_cps_), and API keys (vlx_secret_,
// vlx_publishable_). 32 bytes of entropy → 64 hex chars after the
// prefix, matching the hosted-invoice public token shape.
const costDashboardTokenPrefix = "vlx_pcd_"

// NewCostDashboardToken mints a fresh public cost-dashboard token.
// Uniqueness is enforced at the DB layer via the partial UNIQUE index
// on customers.cost_dashboard_token_hash (migration 0172). The raw token
// is the sole credential for GET /v1/public/cost-dashboard/{token} —
// the operator copies it once at rotate-time; rotation invalidates
// the old token immediately.
func NewCostDashboardToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("cost dashboard token: %w", err)
	}
	return costDashboardTokenPrefix + hex.EncodeToString(raw[:]), nil
}

// HashCostDashboardToken is the deterministic blind index that resolves a
// presented URL token to its customer row without the database ever holding
// the credential. The token carries 256 bits of entropy, so a plain SHA-256
// is sufficient — there is nothing to brute-force and nothing to reverse, so
// a leaked snapshot yields no working dashboard URL.
//
// Unlike the hosted-invoice token there is no encrypted twin: the raw token
// is emitted exactly once, in the rotate response. Nothing re-reads it, so
// storing a reversible copy would add a liability with no reader.
//
// Must match the SQL used by migration 0172's backfill,
// encode(sha256(cost_dashboard_token::bytea),'hex') — the pair is pinned by
// TestHashCostDashboardToken_MatchesSQLBackfill against real Postgres.
func HashCostDashboardToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}
