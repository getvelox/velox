-- 0174: singleton-role leadership as a ROW, not a Postgres session (ADR-114).
--
-- One row per role, seeded here. Every lease decision compares now() to
-- timestamps written by now(); no app clock ever enters a predicate. Every
-- statement the app issues against this table is a single autocommit
-- statement with all state in the row, so PgBouncer transaction mode and
-- RDS Proxy work for the app pool by construction (ADR-114 R1).
--
-- No tenant_id: cluster-scoped, so it sits outside the RLS fence by
-- construction (rls_enumeration_test.go discovers tenant tables by column).
--
-- Unused by the binary until the cutover PR (ADR-114 PR-D) wires the five
-- roles onto it; until then the advisory-lock gate still ships.
CREATE TABLE leader_leases (
    role                TEXT        PRIMARY KEY
                        CHECK (role IN ('billing','dunning','webhook_outbox','email_outbox','webhook_delivery')),
    -- Fencing token: strictly increasing per role for the table's lifetime.
    -- Seeded from epoch-ms so a rollback+re-apply (or a recreated row) lands
    -- ABOVE any token ever issued (tokens grow <= 0.5/s; epoch-ms 1000/s).
    holder_token        BIGINT      NOT NULL CHECK (holder_token > 0),
    holder_id           TEXT        CHECK (holder_id IS NULL OR length(holder_id) BETWEEN 1 AND 200), -- hostname:pid:8hex; NULL = unheld
    acquired_at         TIMESTAMPTZ,
    heartbeat_at        TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,                           -- DB-clock lease end; NULL = unheld
    last_tick_ended_at  TIMESTAMPTZ,                           -- cluster cadence: a role is due when this is older than its interval
    last_tick_holder    TEXT,
    paused_at           TIMESTAMPTZ,                           -- operator pause: survives restarts, it is data
    paused_by           TEXT,
    pause_reason        TEXT,
    CONSTRAINT leader_leases_held_shape CHECK (
        (holder_id IS NULL) = (acquired_at IS NULL)
        AND (holder_id IS NULL) = (heartbeat_at IS NULL)
        AND (holder_id IS NULL) = (expires_at IS NULL)),
    CONSTRAINT leader_leases_lease_after_acquire CHECK (holder_id IS NULL OR expires_at > acquired_at),
    CONSTRAINT leader_leases_pause_shape        CHECK ((paused_at IS NULL) = (paused_by IS NULL)),
    -- A paused role has no holder: a hand-typed pause that forgets to evict
    -- fails here instead of leaving a holder whose claims still pass the fence.
    CONSTRAINT leader_leases_paused_is_unheld   CHECK (paused_at IS NULL OR holder_id IS NULL)
);

INSERT INTO leader_leases (role, holder_token)
SELECT r, (extract(epoch FROM clock_timestamp()) * 1000)::bigint
FROM unnest(ARRAY['billing','dunning','webhook_outbox','email_outbox','webhook_delivery']) AS r;

-- THE FENCE. STABLE => evaluated against the CALLING statement's snapshot as a
-- one-time qual: a claim statement proves the token is live in the same
-- statement. paused => unheld => expires_at IS NULL => false.
CREATE FUNCTION leader_fence(p_role text, p_token bigint) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT EXISTS (
        SELECT 1 FROM leader_leases
         WHERE role = p_role AND holder_token = p_token AND expires_at > now())
$$;

-- Operator pause/unpause as functions: evict the holder + block in one call.
-- NULL result = already in that state.
CREATE FUNCTION leader_pause(p_role text, p_by text, p_reason text) RETURNS boolean
LANGUAGE sql AS $$
    UPDATE leader_leases
       SET paused_at = now(), paused_by = p_by, pause_reason = p_reason,
           holder_id = NULL, acquired_at = NULL, heartbeat_at = NULL, expires_at = NULL
     WHERE role = p_role AND paused_at IS NULL
    RETURNING true
$$;

CREATE FUNCTION leader_unpause(p_role text) RETURNS boolean
LANGUAGE sql AS $$
    UPDATE leader_leases
       SET paused_at = NULL, paused_by = NULL, pause_reason = NULL
     WHERE role = p_role AND paused_at IS NOT NULL
    RETURNING true
$$;

-- Cluster-visible leadership for operators and the metrics scraper.
CREATE VIEW leader_status AS
SELECT role, holder_id, holder_token,
       (expires_at > now()) IS TRUE                                              AS held,
       acquired_at, heartbeat_at, expires_at,
       round(extract(epoch FROM (expires_at - now()))::numeric, 1)              AS expires_in_s,
       last_tick_holder, last_tick_ended_at,
       round(extract(epoch FROM (now() - last_tick_ended_at))::numeric, 1)      AS last_tick_age_s,
       paused_at, paused_by, pause_reason
  FROM leader_leases;

-- Runtime role: DML only, never owner (0001 pattern; default privileges already
-- grant ALL on new tables in every environment, so this states intent).
-- INSERT is required by the ACQUIRE upsert; DELETE/TRUNCATE are never issued.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'velox_app') THEN
        GRANT SELECT, INSERT, UPDATE ON leader_leases TO velox_app;
        GRANT SELECT ON leader_status TO velox_app;
        GRANT EXECUTE ON FUNCTION leader_fence(text, bigint), leader_pause(text, text, text), leader_unpause(text) TO velox_app;
    END IF;
END $$;
