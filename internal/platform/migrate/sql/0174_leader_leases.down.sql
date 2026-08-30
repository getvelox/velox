-- 0174 down: remove the lease table. Rolling back requires the fleet stopped
-- (migrations are a deploy step on a direct connection). Re-applying reseeds
-- tokens from epoch-ms, above any token ever issued (see the up).
DROP VIEW IF EXISTS leader_status;
DROP FUNCTION IF EXISTS leader_unpause(text);
DROP FUNCTION IF EXISTS leader_pause(text, text, text);
DROP FUNCTION IF EXISTS leader_fence(text, bigint);
DROP TABLE IF EXISTS leader_leases;
