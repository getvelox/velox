-- ADR-101 Phase 1: billing_intervals — write-time item lifetimes.
--
-- Policy-applied billable ranges (answers, not questions): each item
-- mutation records, in the SAME tx, the day-grade-decided
-- [starts_at, ends_at) for (item, plan, quantity). The
-- subscription_item_changes fact log stays untouched forever — facts
-- are never backdated by policy. Nothing READS this table in Phase 1
-- (dual-write only); the cycle reader cuts over in Phase 3 behind
-- VELOX_BILLING_INTERVALS_READER.
--
-- The constraints are the design: writer bugs here are silent money,
-- so the two fatal shapes — overlapping ranges and duplicate/negative
-- opens — are LOUD transaction failures, not data.

-- EXCLUDE ... WITH = on a text column requires btree_gist.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE billing_intervals (
    id                   TEXT PRIMARY KEY DEFAULT 'vlx_biv_' || encode(gen_random_bytes(12), 'hex'),
    tenant_id            TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode             BOOLEAN NOT NULL DEFAULT true,
    -- CASCADE is the ADR-086 carve-out from "no deletes": test-clock
    -- teardown hard-deletes subscriptions and everything simulated
    -- under them must go too.
    subscription_id      TEXT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    -- No FK: the item row may be soft-deleted while its closed
    -- intervals remain billing history (same stance as the fact log).
    subscription_item_id TEXT NOT NULL,
    plan_id              TEXT NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
    quantity             BIGINT NOT NULL,
    starts_at            TIMESTAMPTZ NOT NULL,
    ends_at              TIMESTAMPTZ,          -- NULL = open
    -- Which writer minted the row — debugging/parity attribution, not
    -- behavior ('create', 'activate', 'trial_convert', 'add', 'plan',
    -- 'quantity', 'remove', 'cancel', 'splice', 'backfill').
    source               TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT billing_intervals_range_check
        CHECK (ends_at IS NULL OR ends_at >= starts_at),
    -- One open interval per item, declaratively.
    CONSTRAINT billing_intervals_one_open
        EXCLUDE (subscription_item_id WITH =) WHERE (ends_at IS NULL),
    -- Overlap is unrepresentable: two ranges for the same item can
    -- never intersect, whatever future writer bug tries.
    CONSTRAINT billing_intervals_no_overlap
        EXCLUDE USING gist (
            subscription_item_id WITH =,
            tstzrange(starts_at, COALESCE(ends_at, 'infinity'::timestamptz)) WITH &&
        )
);

-- Cycle-close hot path: intervals overlapping a period, per sub.
CREATE INDEX idx_biv_sub_range ON billing_intervals (subscription_id, starts_at);
-- Open-interval lookups (close/transition target) are served by the
-- billing_intervals_one_open exclusion index.

ALTER TABLE billing_intervals ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_intervals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing_intervals FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR (tenant_id = current_setting('app.tenant_id', true)
        AND livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))
);

GRANT ALL ON TABLE billing_intervals TO velox_app;

-- Mode-aware table: livemode is derived from the tx session like every
-- other partitioned table (0021) — explicit INSERT values are overwritten
-- so a buggy producer can't poison a partition.
CREATE TRIGGER set_livemode
    BEFORE INSERT ON billing_intervals
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();

-- ===========================================================================
-- The 0129 trigger's un-delete branch becomes LOUD (ADR-101).
-- ===========================================================================
-- It was written for an un-delete flow that never shipped ("no current
-- flow does this", 0129) and is reachable only via manual SQL. Post-
-- Phase-1, a manual un-delete would resurrect billing with NO interval
-- row — the interval reader would then bill the item nothing, silently.
-- Per no-silent-fallbacks: refuse until a real un-delete flow (with an
-- interval writer) exists.
CREATE OR REPLACE FUNCTION record_subscription_item_change() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO subscription_item_changes
            (tenant_id, livemode, subscription_id, subscription_item_id,
             change_type, to_plan_id, to_quantity, changed_at)
        VALUES
            (NEW.tenant_id, NEW.livemode, NEW.subscription_id, NEW.id,
             'add', NEW.plan_id, NEW.quantity, NEW.created_at);
        RETURN NEW;

    ELSIF TG_OP = 'UPDATE' THEN
        -- Soft delete IS the remove event post-0102.
        IF NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL THEN
            INSERT INTO subscription_item_changes
                (tenant_id, livemode, subscription_id, subscription_item_id,
                 change_type, from_plan_id, from_quantity, changed_at)
            VALUES
                (OLD.tenant_id, OLD.livemode, OLD.subscription_id, OLD.id,
                 'remove', OLD.plan_id, OLD.quantity, NEW.deleted_at);
            RETURN NEW;
        END IF;
        -- Un-delete: REFUSED until a real flow exists (ADR-101). The 0129
        -- version emitted 'add' here so MRR reappearance would not be
        -- silent — but billing_intervals are maintained by Go writers,
        -- and no flow performs un-deletes, so a manual resurrection
        -- would leave the item interval-less and silently unbilled once
        -- the interval reader cuts over. Fail loud instead.
        IF NEW.deleted_at IS NULL AND OLD.deleted_at IS NOT NULL THEN
            RAISE EXCEPTION 'un-deleting subscription_items is not supported: no flow maintains billing_intervals for resurrection (ADR-101)';
        END IF;
        -- Updates to rows that are (and stay) soft-deleted don't move MRR.
        IF NEW.deleted_at IS NOT NULL THEN
            RETURN NEW;
        END IF;
        -- Plan change dominates: if plan_id changed (with or without a quantity
        -- change), emit 'plan' and capture both before/after for a full delta.
        -- A pure quantity change (plan_id unchanged) emits 'quantity'.
        IF NEW.plan_id IS DISTINCT FROM OLD.plan_id THEN
            INSERT INTO subscription_item_changes
                (tenant_id, livemode, subscription_id, subscription_item_id,
                 change_type, from_plan_id, to_plan_id,
                 from_quantity, to_quantity, changed_at)
            VALUES
                (NEW.tenant_id, NEW.livemode, NEW.subscription_id, NEW.id,
                 'plan', OLD.plan_id, NEW.plan_id,
                 OLD.quantity, NEW.quantity,
                 COALESCE(NEW.plan_changed_at, NEW.updated_at));
        ELSIF NEW.quantity IS DISTINCT FROM OLD.quantity THEN
            INSERT INTO subscription_item_changes
                (tenant_id, livemode, subscription_id, subscription_item_id,
                 change_type, from_plan_id, to_plan_id,
                 from_quantity, to_quantity, changed_at)
            VALUES
                (NEW.tenant_id, NEW.livemode, NEW.subscription_id, NEW.id,
                 'quantity', NEW.plan_id, NEW.plan_id,
                 OLD.quantity, NEW.quantity, NEW.updated_at);
        END IF;
        -- Pending-plan scheduling, metadata-only updates: no audit row.
        RETURN NEW;

    ELSIF TG_OP = 'DELETE' THEN
        -- Skip the audit row when the parent subscription itself is being
        -- deleted in the same statement (cascade from DELETE FROM subscriptions).
        IF NOT EXISTS (SELECT 1 FROM subscriptions WHERE id = OLD.subscription_id) THEN
            RETURN OLD;
        END IF;
        INSERT INTO subscription_item_changes
            (tenant_id, livemode, subscription_id, subscription_item_id,
             change_type, from_plan_id, from_quantity, changed_at)
        VALUES
            (OLD.tenant_id, OLD.livemode, OLD.subscription_id, OLD.id,
             'remove', OLD.plan_id, OLD.quantity, now());
        RETURN OLD;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
