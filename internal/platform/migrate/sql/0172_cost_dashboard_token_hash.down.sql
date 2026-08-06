-- Down is LOSSY, deliberately: the raw tokens were dropped and cannot be
-- recovered from a SHA-256. The column is re-added empty, so every existing
-- cost-dashboard URL stops resolving until the operator rotates it (the rotate
-- button is the supported recovery, and rotation was always the documented way
-- to invalidate a link). Losing access to a read-only dashboard is the right
-- side to fail on when rolling back a migration whose whole purpose is to stop
-- leaking replayable credentials.
ALTER TABLE customers ADD COLUMN cost_dashboard_token text;
CREATE UNIQUE INDEX idx_customers_cost_dashboard_token
    ON customers (cost_dashboard_token)
    WHERE cost_dashboard_token IS NOT NULL;

DROP INDEX IF EXISTS idx_customers_cost_dashboard_token_hash;
ALTER TABLE customers DROP COLUMN cost_dashboard_token_hash;
