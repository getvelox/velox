-- Display metadata only — no money meaning. Dropping it reverts invoice
-- unit-price columns to the per-single-unit form; nothing else changes.
ALTER TABLE invoice_line_items DROP COLUMN meter_unit;
