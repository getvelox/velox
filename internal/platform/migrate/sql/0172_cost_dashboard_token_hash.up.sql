-- 0172: stop storing the customer cost-dashboard token in plaintext.
--
-- customers.cost_dashboard_token IS the credential for
-- GET /v1/public/cost-dashboard/{token} — an unauthenticated surface that
-- exposes a customer's usage and spend. Stored plaintext, any read of the
-- customers table (snapshot, replica, backup, a SELECT through an injection)
-- yielded directly-replayable dashboard URLs for every customer at once.
--
-- This is the same fix migration 0107 applied to invoices.public_token, minus
-- the ciphertext half: the hosted-invoice URL must be REBUILT on re-send, so
-- 0107 also kept an AES-GCM copy. The cost-dashboard token is show-once — the
-- rotate response is the only place the raw token is ever emitted, and the
-- dashboard card holds it in component state, never re-fetching it — so a
-- one-way hash is sufficient and a reversible copy would be a liability with
-- no reader.
ALTER TABLE customers ADD COLUMN cost_dashboard_token_hash text;

-- Backfill so existing dashboard URLs keep resolving across the deploy.
-- encode(sha256(token::bytea),'hex') matches customer.HashCostDashboardToken;
-- the pair is pinned by TestHashCostDashboardToken_MatchesSQLBackfill.
UPDATE customers
   SET cost_dashboard_token_hash = encode(sha256(cost_dashboard_token::bytea), 'hex')
 WHERE cost_dashboard_token IS NOT NULL AND cost_dashboard_token <> '';

-- Partial-unique mirrors the index it replaces: rotation must not collide, but
-- the many customers with no dashboard token must not collide on NULL either.
CREATE UNIQUE INDEX idx_customers_cost_dashboard_token_hash
    ON customers (cost_dashboard_token_hash)
    WHERE cost_dashboard_token_hash IS NOT NULL;

DROP INDEX IF EXISTS idx_customers_cost_dashboard_token;
ALTER TABLE customers DROP COLUMN cost_dashboard_token;
