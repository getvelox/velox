package email

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestDispatcherUnknownType_IsRetryable pins HA-readiness hazard 2 (closed
// 2026-08-30): an email_type this binary has no case for is a rolling-deploy
// skew, not a poison payload. It must be classified RETRYABLE so a newer
// replica can claim the row — never ErrPayloadDecode, never permanent.
func TestDispatcherUnknownType_IsRetryable(t *testing.T) {
	ctx := context.Background()
	sender := &recordingDeliverer{}
	d := NewDispatcher(nil, sender, DispatcherConfig{})

	err := d.handle(ctx, rowOf("future_type_from_a_newer_release"))
	if !errors.Is(err, ErrUnknownEmailType) {
		t.Fatalf("want ErrUnknownEmailType, got %v", err)
	}
	if errors.Is(err, ErrPayloadDecode) {
		t.Fatalf("unknown type must not be classified as a payload-decode failure (that is permanent): %v", err)
	}
	if IsPermanentSendError(err) {
		t.Fatalf("unknown type must be retryable, IsPermanentSendError said permanent: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("nothing should be sent for an unknown type, sent %v", sender.sent)
	}
}

// TestIsPermanentSendError_UnknownTypeBeatsBounceScan: the classifier must
// decide on the sentinel before the SMTP-bounce text scan, so a future type
// name that happens to contain a 55x token is still retryable.
func TestIsPermanentSendError_UnknownTypeBeatsBounceScan(t *testing.T) {
	err := fmt.Errorf("%w: 550 5.1.1 user unknown", ErrUnknownEmailType)
	if IsPermanentSendError(err) {
		t.Fatal("ErrUnknownEmailType wrapped with bounce-looking text was classified permanent — the sentinel must be checked before the text scan")
	}
}

// TestDispatcherEveryDeclaredTypeDispatches is the test-time replacement
// for the runtime fail-fast this change removed: every Type* constant the
// producer side can enqueue has a dispatcher case in this binary.
func TestDispatcherEveryDeclaredTypeDispatches(t *testing.T) {
	ctx := context.Background()
	all := []string{
		TypeInvoice, TypePaymentReceipt, TypeDunningWarning, TypeDunningEscalation,
		TypePaymentFailed, TypePaymentSetupRequest, TypePaymentSetupLink,
		TypePasswordReset, TypeMemberInvite, TypeCreditNote,
	}
	for _, typ := range all {
		sender := &recordingDeliverer{}
		d := NewDispatcher(nil, sender, DispatcherConfig{})
		d.SetSettledChecker(&recordingChecker{state: ""}) // invoice live: action-required types deliver
		if err := d.handle(ctx, rowOf(typ)); err != nil {
			t.Errorf("%s: handle returned %v — a declared type with no dispatcher case", typ, err)
			continue
		}
		if len(sender.sent) != 1 || sender.sent[0] != typ {
			t.Errorf("%s: sent %v, want exactly [%s]", typ, sender.sent, typ)
		}
	}
}
