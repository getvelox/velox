# Design: additional_emails + CC coverage — multi-recipient billing emails

Settled 2026-07-06 by a divergent design panel (independent designs
argued from competing lenses — peer parity vs plumbing simplicity —
plus an adversarial judge, wf_f804205b-528). Register item (the tracked
backlog entry this design resolves): "Invoice/receipt/dunning emails go
to exactly one recipient" (fix-now, ~day) + promoted rider (a small
scope item shipped alongside): `POST /v1/credit-notes/{id}/send`
(hours). Peer anchor (what comparable billing platforms do): Orb CCs
the customer's additional_emails on every invoice email; Lago supports
multi-recipient + resend override; Stripe supports additional/CC
recipients on Billing emails.

## Settled decisions

1. **Storage**: `customers.additional_emails TEXT NOT NULL DEFAULT ''`
   (migration 0140) holding the **same-encryptor ciphertext of a JSON
   string array** — encrypted with the same encryptor that already
   protects the sibling `email` column, riding
   encryptCustomer/decryptCustomer (nil encryptor = plaintext JSON). A
   plaintext TEXT[] was rejected: it would leak in a dump exactly the
   PII the primary-email column encrypts, and no DB cardinality CHECK
   can inspect ciphertext. Domain: `Customer.AdditionalEmails []string`.
   Down (the migration's rollback) = plain DROP COLUMN. The
   `customer_email_contacts` table stays phase 3 (trigger:
   per-recipient preferences / contact names /
   per-contact deliverability).

2. **Validation** (customer service, Create + Update): trim →
   `mail.ParseAddress` → **store the normalized lowercased
   `addr.Address`** (ParseAddress accepts display-name forms; storing
   raw input would let display names / CRLF material reach the Cc
   header builder) → case-insensitive dedupe → reject any entry equal
   to the primary email → cap 10. `UpdateInput.AdditionalEmails
   *[]string` (pointer distinguishes omitted from explicit clear).
   Customer PATCH audit records that-the-list-changed (count only, no
   addresses — the email_changed convention).

3. **Transport**: ONE MIME message, primary in `To:`, additional in a
   **visible `Cc:` header** (rendered per-address via
   `(&mail.Address{Address: a}).String()` — the fromHeaderValue
   injection-neutralization pattern), one SMTP transaction with RCPT TO
   for primary + each surviving CC. Rejected: N separate envelopes —
   each email is one outbox row (a DB-queued send drained by the
   dispatcher), so one row with N retry states = duplicate sends on
   retry — and BCC (no peer blind-copies invoices).

4. **deliver() error attribution** (adversarial blindspot fix):
   signature becomes `deliver(ctx, tenantID, to, cc, body)`. Primary
   RCPT rejection (the SMTP server refusing that recipient at the
   RCPT TO step) → hard error (today's semantics; bounce reported for
   the primary). CC RCPT **protocol rejection** (*textproto.Error) →
   WARN + reportBounceIfPermanent(**the CC address**) + drop + continue.
   CC **transport/IO error** → whole send errors and retries (DATA not
   yet sent — retry-safe). CC rejections NEVER propagate as deliver's
   return error. sendRich attributes any returned error to msg.To, so a
   leaked CC 550 would wrongly flip the PRIMARY customer to bounced.

5. **Coverage matrix**: CC-eligible (6) = invoice, payment_receipt,
   dunning_warning, dunning_escalation, payment_failed, credit_note
   (new) — billing documents + billing-state notices. Never-CC (4) =
   payment_setup_request, payment_setup_link (single-use tokenized
   payment-credential URLs — CC = credential spread), password_reset,
   member_invite (personal account credentials). Exclusion is
   STRUCTURAL — the 4 keep their exact signatures, no cc parameter
   exists to misuse.

6. **Suppression** (the do-not-mail check — an address whose customer has bounced or complained):
   primary suppressed → whole send blocked loudly at
   both existing gates (enqueue + dispatch), never downgraded to
   CC-only. CC filtering at DISPATCH time only, inside sendRich only,
   per-address IsSuppressed under one shared 5s ctx; each drop = INFO
   log; send proceeds. No per-CC persistent suppression state in v1
   (bounce on a never-a-customer alias is log-only; stated in ADR).

7. **Plumbing**: append `cc []string` after `to` on exactly the 6
   CC-eligible Send* methods across Sender / OutboxSender /
   EmailDeliverer / the narrow per-domain interfaces. outboxMessage
   gains `Cc []string` + `CreditNoteNumber string`. Recipient
   resolution stays at call sites: `GetCustomerEmail` widens to
   `(email, displayName, additionalEmails, err)` — one impl
   (adapters.go), zero new queries. Rejected: a Recipients struct
   (churns the never-CC types) and resolve-inside-OutboxSender
   (cross-domain import + kills the per-send override).

8. **Operator override**: `POST /v1/invoices/{id}/send` body becomes
   `{email, additional_emails *[]string}` — absent → customer's stored
   list (so legacy request bodies get CC by default — the Orb-parity
   behavior, called out in CHANGELOG); `[]` → primary only; an explicit
   list → validated exact override.

9. **CN (credit note) send rider**: `POST /v1/credit-notes/{id}/send`,
   issued-only (draft/voided → 422). PDF via the assembly shared with
   downloadPDF. New `TypeCreditNote` outbox type (no email_type CHECK
   exists — no migration). New template: subject "Credit note {number}
   from {company}", names the applied invoice, PDF attached, NO CTA
   (call-to-action link) — no hosted CN page exists. Audit
   `domain.AuditActionSend` on resource credit_note (no recipient
   address — GDPR convention), NO outbound webhook. **Timeline blindspot fix**: the hardcoded
   email_type allowlists in OutboxStore.ListByInvoice + ListByCustomer
   gain 'credit_note', plus a web-v2 label.

10. **UI**: comma-separated text inputs (chips deferred as cosmetic):
    customer form "Additional billing emails" field; invoice send
    dialog CC field prefilled + clearable; CreditNotes.tsx Send row
    action on issued CNs.

## Out of scope (named triggers)

contacts table (phase 3); per-CC persistent suppression / auto-removal
(repeated hard-bounces observed); per-email-type routing (DP —
design-partner — ask);
email-sent webhook events + credit_note.* lifecycle family (design
whole when a DP automation asks); hosted CN page (DP ask); auto-email
at finalize (separate register item); CC on the 4 credential types
(permanent — revisit only if the credential model changes); provider
bounce-webhook ingestion; BCC/Reply-To/self-copy; backfill tooling.
