package billing_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// cardFor resolves a chargeable card only for the listed customers; everyone
// else has none, which is the card-less branch of the collector.
type cardFor map[string]bool

func (c cardFor) ResolveForCharge(_ context.Context, _, customerID string) (string, string, error) {
	if c[customerID] {
		return "cus_stripe_" + customerID, "pm_card", nil
	}
	return "", "", nil
}

// The bug, reproduced on the real store and then fixed: 50 card-less invoices
// stay queued (a card added later is charged on the next visit), and the sweep
// read only the 50 oldest queued rows. A newer invoice that could be charged
// was never reached. The sweep now pages through the whole queue, so it is
// charged on the first tick, and only once.
func TestRetryPendingCharges_CardlessHeadDoesNotBlockNewerInvoices(t *testing.T) {
	h := newCollectHarness(t)
	for i := 0; i < 50; i++ {
		h.seed(t, fmt.Sprintf("INV-NOCARD-%02d", i))
	}
	chargeable := h.seed(t, "INV-CARD-1")

	charger := &countingCharger{store: h.invoices}
	e := h.newEng(cardFor{chargeable.CustomerID: true}, charger, nil)

	pages := 0
	charged, errs := e.RetryPendingCharges(h.ctx, 50, func() { pages++ })
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if charged != 1 || charger.count(chargeable.ID) != 1 {
		t.Fatalf("charged = %d, charges of the newer invoice = %d; want 1 and 1 (it sits behind 50 card-less invoices)",
			charged, charger.count(chargeable.ID))
	}
	if pages != 2 {
		t.Errorf("pages = %d, want 2 (50, then 1)", pages)
	}
	queued, err := h.invoices.ListAutoChargePending(h.ctx, domain.InvoiceKeyset{}, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(queued) != 50 {
		t.Errorf("queued after the sweep = %d, want the 50 card-less invoices (they stay queued for charge-on-attach)", len(queued))
	}

	// Later ticks visit the card-less invoices again, but never re-charge.
	for tick := 0; tick < 2; tick++ {
		e.RetryPendingCharges(h.ctx, 50, nil)
	}
	if got := charger.count(chargeable.ID); got != 1 {
		t.Errorf("the paid invoice was charged %d times across 3 ticks, want 1", got)
	}
}

// Rows with the same created_at (a catch-up or a bulk finalize) must all be
// visited: the cursor breaks ties by id. A cursor on created_at alone would
// skip every row sharing the last row's timestamp.
func TestListAutoChargePending_CursorBreaksCreatedAtTiesByID(t *testing.T) {
	h := newCollectHarness(t)
	a := h.seed(t, "INV-TIE-A")
	b := h.seed(t, "INV-TIE-B")
	c := h.seed(t, "INV-TIE-C")
	same := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Pin the shared created_at one row at a time, highest id first. Each
	// write puts a new row version at the end of the table and its index, so
	// storage order becomes the REVERSE of id order. Without the id tie-break
	// in ORDER BY, the first page is then the highest id and the cursor skips
	// the other two.
	ids := []string{a.ID, b.ID, c.ID}
	slices.Sort(ids)
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(h.ctx, `UPDATE invoices SET created_at = $1 WHERE id = $2`, same, ids[i]); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	seen := map[string]bool{}
	var after domain.InvoiceKeyset
	for page := 0; page < 5; page++ {
		got, err := h.invoices.ListAutoChargePending(h.ctx, after, 1)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) == 0 {
			break
		}
		if seen[got[0].ID] {
			t.Fatalf("page %d repeated %s", page, got[0].ID)
		}
		seen[got[0].ID] = true
		after = domain.KeysetOf(got[0])
	}
	for _, inv := range []domain.Invoice{a, b, c} {
		if !seen[inv.ID] {
			t.Errorf("%s was skipped: it shares created_at with the cursor row", inv.ID)
		}
	}
}

// The test-clock queue pages the same way: after the cursor, by (created_at,
// id). Without the cursor every full page would be the first page again, and
// a clock with 100 or more queued invoices would loop until Advance times out.
func TestListAutoChargePendingForClock_PagesAfterTheCursor(t *testing.T) {
	h := newCollectHarness(t)
	const clockID = "vlx_tclk_drain"
	first := h.seed(t, "INV-CLK-1")
	second := h.seed(t, "INV-CLK-2")
	tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(h.ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'drain', now(), 'ready', false)`, clockID, h.tenantID); err != nil {
		t.Fatalf("seed clock: %v", err)
	}
	if _, err := tx.ExecContext(h.ctx, `UPDATE customers SET test_clock_id = $1 WHERE id IN ($2, $3)`, clockID, first.CustomerID, second.CustomerID); err != nil {
		t.Fatalf("pin customers: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	page1, err := h.invoices.ListAutoChargePendingForClock(h.ctx, h.tenantID, clockID, domain.InvoiceKeyset{}, 1)
	if err != nil || len(page1) != 1 {
		t.Fatalf("page 1: %v %v", page1, err)
	}
	page2, err := h.invoices.ListAutoChargePendingForClock(h.ctx, h.tenantID, clockID, domain.KeysetOf(page1[0]), 1)
	if err != nil || len(page2) != 1 {
		t.Fatalf("page 2: %v %v", page2, err)
	}
	if page1[0].ID == page2[0].ID {
		t.Fatalf("page 2 repeated %s: the cursor is ignored", page1[0].ID)
	}
	page3, err := h.invoices.ListAutoChargePendingForClock(h.ctx, h.tenantID, clockID, domain.KeysetOf(page2[0]), 1)
	if err != nil || len(page3) != 0 {
		t.Fatalf("page 3 = %v %v, want empty", page3, err)
	}
}
