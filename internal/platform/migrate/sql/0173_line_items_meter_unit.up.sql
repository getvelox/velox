-- 0173: carry the meter's unit onto usage invoice lines, for display scale.
--
-- The dashboard's Pricing section already renders token rates per-million
-- ("$3.00 / 1M tokens" — the convention every AI provider and rate card
-- uses; web-v2/src/lib/priceDisplay.ts unitScale). Invoice surfaces could
-- not follow it because a line item never carried its meter's unit, so the
-- unit-price column showed the per-token form ("$0.000003") on the exact
-- document the wedge is judged by.
--
-- The line stamps the RAW FACT (the unit string) rather than a resolved
-- scale, so display conventions stay in one place per renderer and an
-- invoice remains a stable document: editing a meter later never re-labels
-- historical lines (same stamped-not-derived reasoning as
-- nominal_unit_amount_decimal, ADR-054 amendment / migration 0142).
ALTER TABLE invoice_line_items ADD COLUMN meter_unit TEXT;

-- Backfill is exact here (unlike 0142's nominal rate, which was
-- unreconstructable for overridden lines): usage lines carry meter_id and
-- the join is 1:1. Non-usage lines have no meter and stay NULL.
UPDATE invoice_line_items li
   SET meter_unit = m.unit
  FROM meters m
 WHERE m.id = li.meter_id
   AND li.meter_id IS NOT NULL
   AND li.line_type = 'usage';
