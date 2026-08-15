package credit_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Property tests for the credit waterfall.
//
// The existing drain tests are example-based and excellent at what they do:
// each constructs a specific portfolio (a promo, a legacy NULL-kind grant, a
// commit block) and pins exactly which block absorbs which cents. They encode
// real regressions — the promotional-first ORDER BY bug among them.
//
// What they cannot do is vary the portfolio. The ordering lives in SQL
// (`ORDER BY (grant_kind IS NOT DISTINCT FROM 'promotional') DESC,
//
//	expires_at NULLS LAST, created_at, id`)
//
// and interacts with expiry filtering, partial consumption and multi-block
// drains. Those interactions are where a waterfall goes wrong, and they are
// combinatorial — the wrong shape to enumerate by hand.
//
// So this generates random portfolios and asserts the invariants that must
// hold for EVERY one. Runs against real Postgres because the ordering being
// tested is the query's, not Go's — a fake would test the wrong thing, which
// is the failure mode that makes an in-memory waterfall test worthless.
//
// Each property below, if violated, is money: credit granted and never
// consumable, credit consumed twice, or credit taken from a block that had
// already expired.

const (
	waterfallTrials       = 60 // portfolios; each is several DB round trips
	waterfallMaxBlocks    = 6
	waterfallMaxGrantCent = 50_000
)

// backdateExpiry lapses an already-created grant. Grants cannot be created
// expired (the service rejects it), so this is how the real "granted then
// expired" state is reached in a test.
func backdateExpiry(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, entryID string, at time.Time) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin backdate: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2 AND tenant_id = $3`,
		at, entryID, tenantID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}

type seededBlock struct {
	id       string
	amount   int64
	promo    bool
	expired  bool
	expires  *time.Time
	consumed int64 // filled in after the drain
}

// TestProperty_Waterfall_ConservesAndRespectsOrder generates a random portfolio
// of credit blocks, drains a random amount through the real ApplyToInvoice
// path, and asserts the invariants that make a credit ledger trustworthy.
func TestProperty_Waterfall_ConservesAndRespectsOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := credit.NewPostgresStore(db)
	svc := credit.NewService(store)
	tenantID := testutil.CreateTestTenant(t, db, "Waterfall Properties")

	// Fixed seed: a failure is reproducible from the trial index alone.
	r := rand.New(rand.NewPCG(0xC0FFEE, 0x0FF1CE))
	now := time.Now().UTC()

	for trial := 0; trial < waterfallTrials; trial++ {
		custID := seedCustomer(t, db, ctx, tenantID, fmt.Sprintf("cus_wf_%03d", trial))

		var blocks []seededBlock
		var liveTotal int64 // credit that SHOULD be drainable
		n := r.IntN(waterfallMaxBlocks) + 1
		for i := 0; i < n; i++ {
			amount := int64(r.IntN(waterfallMaxGrantCent) + 1)
			promo := r.IntN(2) == 0
			expired := r.IntN(4) == 0 // a quarter of blocks are already expired

			in := credit.GrantInput{
				CustomerID:  custID,
				AmountCents: amount,
				Description: fmt.Sprintf("trial %d block %d", trial, i),
			}
			if promo {
				in.GrantKind = domain.GrantKindPromotional
			}
			// The API correctly REFUSES to create an already-expired grant
			// ("dead on arrival"), so an expired block cannot be seeded
			// directly. But expired blocks are a real state — time passes.
			// Seed with a future expiry and backdate it below, which is a
			// faithful simulation of a grant that has since lapsed.
			var exp *time.Time
			if expired || r.IntN(2) == 0 {
				e := now.Add(time.Duration(r.IntN(720)+24) * time.Hour)
				exp = &e
			}
			in.ExpiresAt = exp

			ent, err := svc.Grant(ctx, tenantID, in)
			if err != nil {
				t.Fatalf("trial %d: grant %d: %v", trial, i, err)
			}
			if expired {
				// Backdate the expiry so the block is lapsed at drain time.
				backdateExpiry(t, db, ctx, tenantID, ent.ID, now.Add(-time.Duration(r.IntN(72)+1)*time.Hour))
			}
			blocks = append(blocks, seededBlock{id: ent.ID, amount: amount, promo: promo, expired: expired, expires: exp})
			if !expired {
				liveTotal += amount
			}
		}

		// Drain somewhere between nothing-available and more-than-available,
		// so both the exhaustive and the partial branch are exercised.
		want := int64(r.IntN(int(liveTotal + waterfallMaxGrantCent + 1)))
		if want == 0 {
			want = 1
		}
		invID := seedPayableInvoice(t, db, ctx, tenantID, custID, want, fmt.Sprintf("VLX-WF-%03d", trial))
		applied, err := svc.ApplyToInvoice(ctx, tenantID, custID, invID, want, fmt.Sprintf("VLX-WF-%03d", trial))
		if err != nil {
			t.Fatalf("trial %d: apply: %v", trial, err)
		}

		for i := range blocks {
			blocks[i].consumed = blockConsumed(t, db, ctx, tenantID, blocks[i].id)
		}

		// --- P1: never applies more than asked -------------------------------
		if applied > want {
			t.Fatalf("trial %d: applied %d > requested %d — the customer was over-credited",
				trial, applied, want)
		}

		// --- P2: never applies more than is actually available ---------------
		if applied > liveTotal {
			t.Fatalf("trial %d: applied %d > live credit %d — credit was created from nothing",
				trial, applied, liveTotal)
		}

		// --- P3: exhaustive when it can be ----------------------------------
		// If enough unexpired credit existed, the whole request must be covered.
		// A shortfall here means drainable credit was silently skipped and the
		// customer was billed for balance they held.
		if want <= liveTotal && applied != want {
			t.Fatalf("trial %d: only applied %d of %d with %d live credit available — "+
				"drainable credit was skipped", trial, applied, want, liveTotal)
		}

		// --- P4: conservation ------------------------------------------------
		var totalConsumed int64
		for _, b := range blocks {
			totalConsumed += b.consumed
		}
		if totalConsumed != applied {
			t.Fatalf("trial %d: blocks consumed %d but %d was applied to the invoice — "+
				"the ledger and the invoice disagree", trial, totalConsumed, applied)
		}

		// --- P5: expired credit is never consumed ----------------------------
		for _, b := range blocks {
			if b.expired && b.consumed != 0 {
				t.Fatalf("trial %d: expired block %s consumed %d — expired credit was spent",
					trial, b.id, b.consumed)
			}
		}

		// --- P6: no block is over-consumed -----------------------------------
		for _, b := range blocks {
			if b.consumed > b.amount {
				t.Fatalf("trial %d: block %s consumed %d of %d — consumed more than granted",
					trial, b.id, b.consumed, b.amount)
			}
			if b.consumed < 0 {
				t.Fatalf("trial %d: block %s consumed %d — negative consumption", trial, b.id, b.consumed)
			}
		}

		// --- P7: promotional credit drains first -----------------------------
		// If any paid-class credit was taken, every live promotional block must
		// be fully exhausted. Otherwise the customer's cash-backed balance is
		// being spent while free credit sits unused — the exact defect the
		// promotional-first ORDER BY exists to prevent.
		paidTaken := int64(0)
		for _, b := range blocks {
			if !b.promo && !b.expired {
				paidTaken += b.consumed
			}
		}
		if paidTaken > 0 {
			for _, b := range blocks {
				if b.promo && !b.expired && b.consumed != b.amount {
					t.Fatalf("trial %d: paid credit consumed (%d) while live promotional block %s "+
						"still had %d unused — promotional-first ordering violated",
						trial, paidTaken, b.id, b.amount-b.consumed)
				}
			}
		}
	}
}
