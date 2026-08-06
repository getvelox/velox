# ADR-109: Snapshot the bill-to and supplier blocks onto issued documents

**Date:** 2026-08-05
**Status:** Proposed (amended 2026-08-05 — writer site-set corrected after enumeration)

## Context

Walking FLOW CU1 surfaced this. Invoice `VLX-000145` was already
`finalized`. Editing the customer's billing profile afterwards — a new
`address_line1`, then a corrected VAT number — changed the PDF that
`GET /v1/invoices/{id}/pdf` returns for that finalized invoice:

```
BILL TO
U1 Failed-Pay GmbH
Unter den Linden 77        <- typed AFTER the invoice was finalized
Berlin 10117
DE
Tax ID: DE123456789        <- also editable after finalization
```

Nothing is snapshotted. Every render site rebuilds the document from
whatever the customer and tenant rows say at request time:

| Site | What it re-derives live |
| --- | --- |
| `internal/invoice/handler.go:2500` | API PDF download |
| `internal/invoice/handler.go:1021` | PDF attached to outbound email |
| `internal/hostedinvoice/handler.go:298` | public hosted-invoice HTML bill-to |
| `internal/hostedinvoice/handler.go:421` | public hosted-invoice PDF |
| `internal/creditnote/handler.go:300` | credit-note HTML bill-to |
| `internal/creditnote/handler.go:373`, `:464` | credit-note PDFs |
| `internal/invoice/pdf_context.go:57` | supplier block, from tenant settings |
| `internal/api/adapters.go:606` | display name on outbound email |

`grep -rn "RenderPDF(" internal` returns only call sites — no table or
column holds a *canonical* rendered document.

**Correction (2026-08-05):** an earlier draft of this ADR said "no
rendered PDF is persisted". That is wrong. `internal/email/outbox_sender.go:91`
carries `PDF []byte` and `SendInvoice` (:166-178) writes the rendered bytes
into `email_outbox.payload` JSONB, with no purge job — so every
operator-sent invoice PDF is already retained indefinitely. It does not
change the decision (that copy is a send artifact, not a canonical
document, and nothing renders *from* it), but it does mean a
pre-snapshot record of what a customer actually received already exists
and is worth consulting during any dispute.

Consequences, in order of severity:

1. **The document a customer received stops matching the document they
   can download.** The emailed PDF was rendered from the profile as it
   was that day; the hosted link re-renders from the profile as it is
   today. Same invoice number, two different legal documents.
2. **The buyer VAT number on a finalized invoice is mutable.** It is a
   plain profile field read live at render time.
   *Refined 2026-08-05:* the legend's **presence** is already safe —
   `tax_reverse_charge` and `tax_exempt_reason` are frozen on the
   invoice row (`internal/invoice/pdf.go:746`). What is still live is
   the legend's **wording**: `reverseChargeLegend` (`pdf.go:95-103`)
   branches through `isIndianContext` (`pdf.go:125-137`) on the live
   buyer country and supplier tax-id type, so an address edit can flip
   an issued invoice between the CGST §9(3)/9(4) wording and the EU
   Art. 196 wording.
3. **Supplier-side drift is the same class.** Rename the company or edit
   its address in settings and every historical invoice re-renders under
   the new letterhead.

Nothing here is a hypothetical: items 1 and 2 were both reproduced by
hand against `VLX-000145` during the walk.

### Industry anchor

**Stripe — snapshots the fields onto the invoice at finalization.**
Verified empirically in the sandbox rather than quoted, because the
Invoice-object reference does not spell the behaviour out. Created a
customer at `Snapshot A GmbH / Alpha Strasse 1 / Berlin`, finalized an
invoice, then mutated the customer:

```
BEFORE:   Snapshot A GmbH | Alpha Strasse 1 Berlin   (status paid)
--- customer updated to Snapshot B AG / Omega Strasse 99 / Hamburg ---
AFTER :   Snapshot A GmbH | Alpha Strasse 1 Berlin
CUSTOMER: Snapshot B AG   | Omega Strasse 99 Hamburg
```

The finalized invoice kept `customer_name` and `customer_address`; the
Customer object moved. The last line is the negative control — it proves
the mutation landed and the invoice's stability is real, not a failed
write. (Probe objects deleted afterwards.)

**Lago — freezes the artifact instead of the fields.** Its invoice object
carries only a `customer` reference, with no duplicated address, so on
the object axis Lago looks like Velox. The difference is that the PDF is
generated once and persisted: `file_url` "contains the URL that provides
direct access to the invoice PDF file", and the download endpoint returns
empty with an `invoice.generated` webhook when the file still has to be
produced. The issued document is a stored file, not a live re-render.

Two platforms, two mechanisms, same guarantee: **what was issued stays
issued.** Velox has neither mechanism — it is the only one of the three
where a finalized document is a live view over mutable rows.

## Decision

Snapshot the bill-to and supplier blocks onto the invoice at the moment
it leaves draft, and render issued documents from the snapshot.

Add to `invoices` (new migration): `bill_to_name`, `bill_to_line1`,
`bill_to_line2`, `bill_to_city`, `bill_to_state`, `bill_to_postal_code`,
`bill_to_country`, `bill_to_tax_id`, `bill_to_tax_id_type`,
`bill_to_tax_status`, plus the supplier block
(`supplier_name`, `supplier_address_*`, `supplier_tax_id`). All
nullable — a NULL snapshot means "issued before this ADR".

Write them in the same transaction that sets the invoice to `finalized`,
alongside the existing finalize writes. Credit notes inherit the
snapshot from the invoice they reference rather than re-reading the
customer.

Render sites prefer the snapshot and fall back to the live profile only
when it is NULL, so historical rows keep rendering exactly as they do
today. `internal/invoice/pdf_context.go` is the natural home for the
resolve-with-fallback helper.

**Correction (2026-08-05):** an earlier draft claimed the invoice and
credit-note paths "already route through" it. They do not.
`internal/creditnote/handler.go:291` (`assemblePDFContext`) is a
hand-rolled duplicate with its own `BillToInfo`/`CompanyInfo` types
(`internal/creditnote/pdf.go:21,40`) and never calls
`invoice.BuildPDFContext`. Sharing the helper therefore costs either a
new cross-domain edge (`internal/arch/boundaries_test.go` gate) or
duplicated resolve logic — a real decision, not a free reuse.

Draft invoices keep reading live — a draft is not an issued document,
and an operator fixing an address before finalizing must see the fix.

## Why this design

**Snapshot columns, not a stored PDF.** Freezing the bytes (Lago's
mechanism) also freezes template bugs and layout fixes into every
historical document, and it needs an object store that Velox does not
currently run. Snapshotting the *inputs* keeps the renderer free to
improve while the legal content stays fixed. It is also the mechanism
the closer anchor (Stripe) uses.

**At finalization, not at creation.** Draft is the editing surface. ADR-058
and the existing finalize path already treat the draft→finalized
transition as the "this is now real" boundary; document identity belongs
on the same boundary rather than a new one.

**Nullable with fallback, no backfill.** Every invoice that exists today
was rendered live, so there is no historical truth to backfill *to* — the
profile as it stands is the only bill-to those rows have ever had.
Inventing a snapshot for them would fabricate a document state that never
existed. Per `feedback_no_speculative_backfill`, the columns start NULL
and only new finalizations populate them.

## Alternatives considered

**Block billing-profile edits once the customer has finalized invoices.**
Rejected: punishes the common, legitimate case (a customer genuinely
moves) to protect the historical one, and there is no correct answer for
"which invoice's address wins" when the profile is shared across many.

**Persist the rendered PDF at finalization.** Rejected for the reasons
above, and it doubles the surface — the hosted HTML view would still
re-render live unless it too were frozen, so the HTML and PDF could
disagree.

**Version the billing profile and have invoices reference a version.**
Rejected as heavier than the problem: it introduces a second lifecycle
to reason about, when the invoice only ever needs the values as of one
instant. Reach for it if per-profile history becomes a product ask in
its own right.

## Consequences

- A finalized invoice's PDF stops changing. The emailed copy and the
  hosted copy agree for the life of the document.
- The buyer VAT number and tax legend on an issued invoice become
  immutable; correcting them requires the existing credit-note path,
  which is the accounting-correct remedy.
- Invoices finalized before this ships keep their current live-render
  behaviour, and the fallback branch is load-bearing for them
  indefinitely. It needs a test, not just a comment.
- Money-path playbook: the finalize writer's full site-set must be
  re-enumerated during implementation. This ADR names the *readers*;
  the writers are the risk, and any finalize path that does not stamp
  the snapshot silently produces a NULL-snapshot invoice that looks
  correct until the profile changes.

## Amendment 2026-08-05 — the writer site-set above is wrong

The enumeration ran, and the decision survives it. The *site-set* does
not. Three corrections, each load-bearing enough to have shipped a
silently-broken snapshot:

**1. There is no Go-level finalize chokepoint.** "Stamp it in the
finalize writer" assumes one. **Six** writers produce a
`status='finalized'` invoice and **five never pass through
`invoice.Service.Finalize` at all** — they are *born finalized*:
cycle close (`billing/engine.go:3147`), day-1/trial-end/activation
(`engine.go:3673`), final-on-immediate-cancel (`engine.go:4377`),
usage thresholds (`threshold_scan.go:722`), and proration/plan-swap
(`subscription/handler.go:2360`). Only operator finalize and
engine-draft finalize go through the service.

The real chokepoint is one layer down: **`invoice/postgres.go:1773`
`createWithLineItemsInTx`**, which all five born-finalized writers
funnel through — and which already mints the public token at :1764 for
exactly this "every born-finalized invoice needs it" reason. Stamp
there, plus the two draft→finalized UPDATEs (`FinalizeWithDates:736`,
`updateStatusInTx:670`). Three SQL sites instead of chasing thirteen
Go callers.

**2. `subscription/handler.go` has no customer-profile dependency
wired.** The proration writer lives in a package that cannot read a
billing profile today, so the snapshot cannot simply be assembled at
each call site — another argument for the store layer.

**3. Four ways it would fail OPEN** — i.e. look correct and quietly
re-read live data, the exact failure this ADR exists to stop:

- *`invCols` COALESCE.* The file's prevailing style
  (`postgres.go:185-203`) is `COALESCE(col,'')`. Written that way NULL
  collapses to `''` and the "snapshot absent → fall back to live"
  branch can **never** fire. The new columns must read as true NULL.
- *livemode RLS asymmetry.* `customer_billing_profiles` carries a
  livemode-scoped isolation policy (`0020_test_mode.up.sql:118-142`)
  while `tenant_settings` is mode-neutral
  (`0006_close_rls_bypass.up.sql`). A snapshot assembled under the
  wrong mode reads empty rather than erroring.
- *partial reader coverage.* Switching only `BuildPDFContext` leaves
  `hostedinvoice/handler.go:293-311` and
  `web-v2/src/pages/InvoiceDetail.tsx:854-916` on live rows, so the PDF
  freezes while the hosted HTML the customer actually opens keeps
  mutating.
- *twin-INSERT drift.* `postgres.go:309 CreateAudited` is a second
  hand-maintained INSERT with its own column list, already missing the
  public-token mint. Draft-only today, so it looks safe — until
  something is born final through it.

**Already shipped and worth fixing in the same PR:** `invoice/pdf.go`
prints the buyer's VAT **twice from two eras** — live `billTo.TaxID`
at :488 and frozen `inv.TaxID` at :604. Whatever the snapshot decides,
those two must agree.

**Open questions for the implementer**, both scope rather than
approach: does `bill_to_email` belong in the snapshot (printed at
`pdf.go:495-499`, shown at `hostedinvoice/handler.go:296`, but omitted
from this ADR's field list), and do `supplier_email` /
`supplier_phone` (printed on every invoice at `pdf.go:381-386`,
likewise omitted).

**4. Four more sites the corrected plan still missed**, found by an
adversarial pass over it:

- **A third `INSERT INTO invoices`** lives outside `internal/invoice`
  entirely — `cmd/velox-migrate-safety/seed.go:224`, writing
  `'finalized'` as a SQL literal. The "only four SQL statements, all in
  internal/invoice" claim above is therefore wrong as stated; it is true
  only of the production paths.
- **`invoices.tax_id` has three writers, not one.**
  `internal/invoice/postgres.go:1650 UpdateTaxAtomic` writes it too, and
  at :1567 it opens **its own** transaction — so the "the snapshot
  shares the invoice's fate because every stamp site is in-tx" argument
  does not hold on the manual-finalize path.
- **`RETURNING` runs before the stamp.**
  `createWithLineItemsInTx:1774` scans the row from `RETURNING invCols`
  on the INSERT. A follow-up UPDATE means the `domain.Invoice` handed
  back to callers carries `bill_to_snapshot_at = NULL` even though the
  row is stamped — so anything that renders from the returned value,
  rather than re-reading, silently takes the fallback path. Fold the
  stamp into the INSERT or re-scan.
- **Coordinator-tx blast radius.** Nine distinct transactions
  (`subscription/postgres.go:118/:339/:786/:889`,
  `subscription/handler.go:1510/:1592/:1709`) will now fail **closed**
  if the stamp errors — a snapshot problem could abort a subscription
  create or cancel. That trade needs stating in the risks section, not
  discovering in production.

**And one interaction worth pausing on:** `RetryCustomerDataErrors`
(`internal/invoice/service.go:1813`) fires **on billing-profile save**
and can auto-finalize invoices (`service.go:1645`). So the very action
that changes a bill-to is itself a finalize trigger — post-fix, saving a
corrected address would stamp the *new* profile onto invoices that had
been waiting on that correction. That is arguably the desired outcome,
but it must be a decision rather than an accident.

Migration number claimed by the enumeration:
**0171_invoice_issued_document_snapshot** (0170 is the highest on
origin/main; 0166 is taken by an unmerged branch and must not be
reused).

## Status note

Proposed, not built. Found mid-walk during FLOW CU1. The site-set has
now been enumerated and adversarially reviewed (see the amendment
above), so the remaining work is the build itself: one migration, three
SQL-layer stamp sites, the reader fallback, and the render sites —
including the hosted HTML, not just the PDF.
