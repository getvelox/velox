# ADR-109: Snapshot the bill-to and supplier blocks onto issued documents

**Date:** 2026-08-05
**Status:** Proposed

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

Nothing is snapshotted and no rendered PDF is persisted. Every render
site rebuilds the document from whatever the customer and tenant rows
say at request time:

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

`grep -rn "RenderPDF(" internal` returns only call sites; there is no
table, column, or object store holding a rendered document.

Consequences, in order of severity:

1. **The document a customer received stops matching the document they
   can download.** The emailed PDF was rendered from the profile as it
   was that day; the hosted link re-renders from the profile as it is
   today. Same invoice number, two different legal documents.
2. **The buyer VAT number on a finalized invoice is mutable.** It is a
   plain profile field, and `tax_status` drives the reverse-charge
   legend, so an edit can retroactively change both the buyer
   registration and the tax legend printed on an already-issued invoice.
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
today. `internal/invoice/pdf_context.go` is the natural place for the
resolve-with-fallback helper, since both the invoice and credit-note
paths already route through it.

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

## Status note

Proposed, not built. Found mid-walk during FLOW CU1; the fix is a
migration plus a change to the finalize writer and eight render sites,
which is its own PR and (per the money-path playbook) its own
site-set enumeration and adversarial panel. The walk continued rather
than absorbing that work inline.
