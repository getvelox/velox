-- 0176 down: drop the cooldown column; the view cannot lose columns via
-- CREATE OR REPLACE, so recreate it as 0174 defined it (grant preserved
-- explicitly — DROP VIEW discards ACLs).
DROP VIEW IF EXISTS leader_status;
CREATE VIEW leader_status AS
SELECT role, holder_id, holder_token,
       (expires_at > now()) IS TRUE                                              AS held,
       acquired_at, heartbeat_at, expires_at,
       round(extract(epoch FROM (expires_at - now()))::numeric, 1)              AS expires_in_s,
       last_tick_holder, last_tick_ended_at,
       round(extract(epoch FROM (now() - last_tick_ended_at))::numeric, 1)      AS last_tick_age_s,
       paused_at, paused_by, pause_reason
  FROM leader_leases;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'velox_app') THEN
        GRANT SELECT ON leader_status TO velox_app;
    END IF;
END $$;
ALTER TABLE leader_leases DROP COLUMN IF EXISTS not_before;
