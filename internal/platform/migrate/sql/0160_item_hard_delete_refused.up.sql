-- ADR-101 Phase 2: direct hard-DELETE of a subscription_items row is
-- REFUSED, like un-delete (0159). No flow issues one (RemoveItem
-- soft-deletes; test-clock teardown deletes the parent subscription,
-- whose cascade this trigger already skips) — so a direct DELETE is
-- manual SQL, and it would strand the item's billing_intervals rows
-- with no owner: the interval reader would keep billing a row-less
-- item (found as fact-log residue on pre-0102 walkthrough data, where
-- the era's hard deletes left 'add' facts that legacy-bill a phantom
-- month). Cascade deletes (parent subscription gone) stay silent —
-- billing_intervals cascades away with them.
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
        -- Un-delete: REFUSED (0159, ADR-101).
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
        -- Cascade from DELETE FROM subscriptions: parent is already gone
        -- in this statement; billing_intervals cascades away too. Silent.
        IF NOT EXISTS (SELECT 1 FROM subscriptions WHERE id = OLD.subscription_id) THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'hard-deleting subscription_items is not supported: use soft delete (RemoveItem) — no flow maintains billing_intervals for a vanished row (ADR-101)';
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
