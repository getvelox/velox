-- 0169: the two credit_notes lookups every refund path leans on, unindexed
-- until now (found during the 2026-08-04 refund-architecture review).
--
-- invoice_id: the over-refund ceiling, the invoice page's CN list, and the
-- PDF's post-payment section all scan WHERE invoice_id = $1; the only
-- existing indexes are the number UNIQUE and two partial recovery indexes.
--
-- stripe_refund_id: the refund-webhook writer matches WHERE stripe_refund_id
-- = $1 — a tenant-wide scan on every refund event. Partial: most CNs carry
-- no refund leg, and '' is the no-id sentinel.
CREATE INDEX IF NOT EXISTS idx_credit_notes_invoice_id
    ON credit_notes (invoice_id);

CREATE INDEX IF NOT EXISTS idx_credit_notes_stripe_refund_id
    ON credit_notes (stripe_refund_id)
    WHERE stripe_refund_id IS NOT NULL AND stripe_refund_id <> '';
