package invoice

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// fakeSettingsReader returns a fixed tenant net-term default, or err.
type fakeSettingsReader struct {
	netTerms int
	err      error
}

func (f fakeSettingsReader) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{NetPaymentTerms: f.netTerms}, f.err
}

func intPtr(n int) *int { return &n }

// Net payment terms on a manual invoice: an explicit value (including 0 =
// "Due on receipt") is honored; an omitted value falls back to the tenant's
// configured terms, 0 included (P24). Pre-fix the field was a plain int, so an
// explicit 0 was indistinguishable from "unset" and silently became Net 30 —
// the composer's "Due on receipt" option didn't work.
func TestCreate_NetPaymentTermResolution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name     string
		input    *int
		settings TenantSettingsReader
		want     int
	}{
		{"explicit Due-on-receipt (0) is honored, not coerced to 30", intPtr(0), fakeSettingsReader{netTerms: 14}, 0},
		{"explicit value is honored over the tenant default", intPtr(45), fakeSettingsReader{netTerms: 14}, 45},
		{"omitted falls back to the tenant default", nil, fakeSettingsReader{netTerms: 14}, 14},
		{"omitted with no settings reader defaults to 30", nil, nil, 30},
		{"omitted with tenant Due-on-receipt (0) is honored", nil, fakeSettingsReader{netTerms: 0}, 0},
		{"explicit negative is clamped to 0", intPtr(-5), nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := NewService(newMemStore(), nil, newMemNumberer())
			if tc.settings != nil {
				svc.SetTenantSettingsReader(tc.settings)
			}
			inv, err := svc.Create(ctx, "t1", CreateInput{
				CustomerID:         "cus_1",
				NetPaymentTermDays: tc.input,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if inv.NetPaymentTermDays != tc.want {
				t.Errorf("net_payment_term_days: got %d, want %d", inv.NetPaymentTermDays, tc.want)
			}
			// due_at must equal issued_at + the resolved term.
			if inv.IssuedAt == nil || inv.DueAt == nil {
				t.Fatalf("expected issued_at and due_at to be set")
			}
			wantDue := inv.IssuedAt.AddDate(0, 0, tc.want)
			if !inv.DueAt.Equal(wantDue) {
				t.Errorf("due_at: got %v, want issued+%dd (%v)", inv.DueAt, tc.want, wantDue)
			}
		})
	}
}

// A settings read failure fails the create instead of guessing Net 30, and
// spends no invoice number (P24).
func TestCreate_NetTermsUnreadableFailsCreate(t *testing.T) {
	t.Parallel()
	numberer := newMemNumberer()
	svc := NewService(newMemStore(), nil, numberer)
	svc.SetTenantSettingsReader(fakeSettingsReader{err: errors.New("settings read: connection reset")})

	if _, err := svc.Create(context.Background(), "t1", CreateInput{CustomerID: "cus_1"}); err == nil {
		t.Fatal("create succeeded with unreadable tenant settings; want an error")
	}
	if numberer.next != 0 {
		t.Errorf("an invoice number was spent before the failure (%d allocated)", numberer.next)
	}
}
