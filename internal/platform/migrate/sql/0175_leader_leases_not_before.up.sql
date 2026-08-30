-- 0175: separate cadence from observability on leader_leases (sweep 2026-08-30 S5).
--
-- Until now a tick that LOST its lease (heartbeat timeout, takeover, pause)
-- was released by stamping last_tick_ended_at = now() - interval + cooldown so
-- the role would be due again after a bounded cooldown — but last_tick_* is
-- also what leader_status and velox_leader_last_tick_age_seconds report as
-- "the last COMPLETED tick", so an abandoned tick read as a completion by the
-- holder that abandoned it. The cooldown now lives in its own column.
--
ALTER TABLE leader_leases ADD COLUMN not_before TIMESTAMPTZ; -- cooldown after a lost tick; NULL = none
COMMENT ON COLUMN leader_leases.not_before IS 'Acquire refuses until this instant (set after a lost tick, cleared by a completed one). Cadence only — never a completion.';

-- CREATE OR REPLACE keeps the existing columns and their order and appends.
CREATE OR REPLACE VIEW leader_status AS
SELECT role, holder_id, holder_token,
       (expires_at > now()) IS TRUE                                              AS held,
       acquired_at, heartbeat_at, expires_at,
       round(extract(epoch FROM (expires_at - now()))::numeric, 1)              AS expires_in_s,
       last_tick_holder, last_tick_ended_at,
       round(extract(epoch FROM (now() - last_tick_ended_at))::numeric, 1)      AS last_tick_age_s,
       paused_at, paused_by, pause_reason,
       not_before,
       round(extract(epoch FROM (not_before - now()))::numeric, 1)              AS not_before_in_s
  FROM leader_leases;
