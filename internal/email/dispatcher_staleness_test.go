package email

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recordingDeliverer counts sends per type; every send succeeds.
type recordingDeliverer struct{ sent []string }

func (r *recordingDeliverer) SendInvoice(_ context.Context, _, _ string, _ []string, _, _ string, _ int64, _ string, _ []byte, _ string) error {
	r.sent = append(r.sent, TypeInvoice)
	return nil
}
func (r *recordingDeliverer) SendPaymentReceipt(_ context.Context, _, _ string, _ []string, _, _ string, _ int64, _, _ string) error {
	r.sent = append(r.sent, TypePaymentReceipt)
	return nil
}
func (r *recordingDeliverer) SendDunningWarning(_ context.Context, _, _ string, _ []string, _, _ string, _, _ int, _, _, _ string) error {
	r.sent = append(r.sent, TypeDunningWarning)
	return nil
}
func (r *recordingDeliverer) SendDunningEscalation(_ context.Context, _, _ string, _ []string, _, _ string, _ domain.DunningEscalationOutcome, _ string) error {
	r.sent = append(r.sent, TypeDunningEscalation)
	return nil
}
func (r *recordingDeliverer) SendPaymentFailed(_ context.Context, _, _ string, _ []string, _, _, _, _ string) error {
	r.sent = append(r.sent, TypePaymentFailed)
	return nil
}
func (r *recordingDeliverer) SendPaymentSetupRequest(_ context.Context, _, _, _, _ string, _ int64, _, _ string, _ bool) error {
	r.sent = append(r.sent, TypePaymentSetupRequest)
	return nil
}
func (r *recordingDeliverer) SendPaymentSetupLink(_ context.Context, _, _, _, _, _ string) error {
	r.sent = append(r.sent, TypePaymentSetupLink)
	return nil
}
func (r *recordingDeliverer) SendPasswordReset(_ context.Context, _, _, _, _ string) error {
	r.sent = append(r.sent, TypePasswordReset)
	return nil
}
func (r *recordingDeliverer) SendMemberInvite(_ context.Context, _, _, _, _, _ string) error {
	r.sent = append(r.sent, TypeMemberInvite)
	return nil
}
func (r *recordingDeliverer) SendCreditNote(_ context.Context, _, _ string, _ []string, _, _, _ string, _ int64, _ string, _ []byte) error {
	r.sent = append(r.sent, TypeCreditNote)
	return nil
}

// recordingChecker records consults and returns a scripted answer.
type recordingChecker struct {
	consulted []string
	// state is the terminal state to report: "" = live, else
	// paid/voided/uncollectible/gone.
	state string
	err   error
}

func (c *recordingChecker) InvoiceTerminalState(_ context.Context, _, invoiceNumber string) (string, error) {
	c.consulted = append(c.consulted, invoiceNumber)
	return c.state, c.err
}

func rowOf(emailType string) OutboxRow {
	return OutboxRow{
		ID: "emob_1", TenantID: "t1", EmailType: emailType,
		Payload: map[string]any{"to": "c@x.test", "customer_name": "C", "invoice_number": "NIM-9"},
	}
}

// TestDispatcherStalenessGate locks the 2026-07-19 FLOW E finding: an
// action-required email whose invoice settled while queued (SMTP outage)
// must be SKIPPED, not delivered — "update your payment method" after the
// customer paid erodes trust. Informational types always deliver, and a
// checker failure fails OPEN (a DB blip must never eat customer mail).
func TestDispatcherStalenessGate(t *testing.T) {
	ctx := context.Background()

	t.Run("obsolete action-required row returns ErrEmailObsolete, nothing sent", func(t *testing.T) {
		for _, typ := range []string{TypePaymentSetupRequest, TypePaymentFailed, TypeDunningWarning, TypeDunningEscalation} {
			sender := &recordingDeliverer{}
			d := NewDispatcher(nil, sender, DispatcherConfig{})
			d.SetSettledChecker(&recordingChecker{state: "paid"})
			err := d.handle(ctx, rowOf(typ))
			if !errors.Is(err, ErrEmailObsolete) {
				t.Errorf("%s: want ErrEmailObsolete, got %v", typ, err)
			}
			if len(sender.sent) != 0 {
				t.Errorf("%s: obsolete row was SENT — the exact bug this gate closes", typ)
			}
		}
	})

	t.Run("uncollectible does NOT mute the escalation that announces it", func(t *testing.T) {
		// mark_uncollectible marks the invoice uncollectible in the same
		// breath as it enqueues this email, so a blanket settled-gate
		// skipped it every time and the customer was never told (FLOW I6
		// walk, VLX-000016).
		//
		// The SETUP LINK is the second exemption, added 2026-08-05. This
		// test used to assert it stayed muted, on the reasoning that
		// "asking for a card on a written-off invoice is still noise" —
		// TRUE when written, because ADR-110 records that Velox then
		// "could not charge a written-off invoice at all". The amendment
		// that shipped operator-driven recovery retired that premise: a
		// card attached now is exactly what unblocks the charge, and the
		// operator cannot proceed without one. payment_failed and
		// dunning_warning stay muted below — those really are stale on a
		// written-off invoice — which is what keeps the exemption narrow.
		sender := &recordingDeliverer{}
		d := NewDispatcher(nil, sender, DispatcherConfig{})
		d.SetSettledChecker(&recordingChecker{state: "uncollectible"})
		if err := d.handle(ctx, rowOf(TypeDunningEscalation)); err != nil {
			t.Fatalf("escalation must deliver on uncollectible: %v", err)
		}
		if len(sender.sent) != 1 {
			t.Fatal("the escalation announcing the write-off was skipped — the customer is never told")
		}
		// The setup link now delivers: it is the recovery on-ramp.
		s1 := &recordingDeliverer{}
		d1 := NewDispatcher(nil, s1, DispatcherConfig{})
		d1.SetSettledChecker(&recordingChecker{state: "uncollectible"})
		if err := d1.handle(ctx, rowOf(TypePaymentSetupRequest)); err != nil {
			t.Fatalf("setup link must deliver on uncollectible — the operator cannot charge a written-off invoice until a card exists: %v", err)
		}
		if len(s1.sent) != 1 {
			t.Fatal("the setup link that unblocks recovery was skipped")
		}
		for _, typ := range []string{TypePaymentFailed, TypeDunningWarning} {
			s2 := &recordingDeliverer{}
			d2 := NewDispatcher(nil, s2, DispatcherConfig{})
			d2.SetSettledChecker(&recordingChecker{state: "uncollectible"})
			if err := d2.handle(ctx, rowOf(typ)); !errors.Is(err, ErrEmailObsolete) {
				t.Errorf("%s on an uncollectible invoice must stay muted, got %v", typ, err)
			}
		}
		// paid/voided still mute the escalation itself.
		for _, st := range []string{"paid", "voided"} {
			s3 := &recordingDeliverer{}
			d3 := NewDispatcher(nil, s3, DispatcherConfig{})
			d3.SetSettledChecker(&recordingChecker{state: st})
			if err := d3.handle(ctx, rowOf(TypeDunningEscalation)); !errors.Is(err, ErrEmailObsolete) {
				t.Errorf("escalation on a %s invoice must stay muted, got %v", st, err)
			}
		}
	})

	t.Run("live invoice sends normally", func(t *testing.T) {
		sender := &recordingDeliverer{}
		d := NewDispatcher(nil, sender, DispatcherConfig{})
		d.SetSettledChecker(&recordingChecker{state: ""})
		if err := d.handle(ctx, rowOf(TypePaymentSetupRequest)); err != nil {
			t.Fatalf("send: %v", err)
		}
		if len(sender.sent) != 1 {
			t.Error("live action-required row must send")
		}
	})

	t.Run("checker error fails OPEN — the email still sends", func(t *testing.T) {
		sender := &recordingDeliverer{}
		d := NewDispatcher(nil, sender, DispatcherConfig{})
		d.SetSettledChecker(&recordingChecker{err: errors.New("db blip")})
		if err := d.handle(ctx, rowOf(TypePaymentFailed)); err != nil {
			t.Fatalf("fail-open send: %v", err)
		}
		if len(sender.sent) != 1 {
			t.Error("checker error must not eat customer mail")
		}
	})

	t.Run("informational types never consult the checker", func(t *testing.T) {
		for _, typ := range []string{TypeInvoice, TypePaymentReceipt} {
			sender := &recordingDeliverer{}
			checker := &recordingChecker{state: "paid"}
			d := NewDispatcher(nil, sender, DispatcherConfig{})
			d.SetSettledChecker(checker)
			if err := d.handle(ctx, rowOf(typ)); err != nil {
				t.Fatalf("%s: %v", typ, err)
			}
			if len(checker.consulted) != 0 {
				t.Errorf("%s: informational mail must deliver regardless of settlement", typ)
			}
			if len(sender.sent) != 1 {
				t.Errorf("%s: not sent", typ)
			}
		}
	})

	t.Run("nil checker leaves the pre-gate behavior", func(t *testing.T) {
		sender := &recordingDeliverer{}
		d := NewDispatcher(nil, sender, DispatcherConfig{})
		if err := d.handle(ctx, rowOf(TypePaymentSetupRequest)); err != nil {
			t.Fatalf("nil checker: %v", err)
		}
		if len(sender.sent) != 1 {
			t.Error("nil checker must send as before")
		}
	})
}
