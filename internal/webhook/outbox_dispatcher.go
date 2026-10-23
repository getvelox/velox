package webhook

import (
	"context"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
)

// OutboxDispatcher satisfies domain.EventDispatcher by persisting each event
// as a pending row in webhook_outbox. The Dispatcher worker drains it.
//
// This is the durable drop-in replacement for calling Service.Dispatch
// directly: producers get persist-before-return semantics, and the actual
// HTTP delivery is done by the worker asynchronously — same as today, just
// with a durable queue in between.
type OutboxDispatcher struct {
	outbox *OutboxStore
}

func NewOutboxDispatcher(outbox *OutboxStore) *OutboxDispatcher {
	return &OutboxDispatcher{outbox: outbox}
}

// Dispatch enqueues a pending outbox row. Returns an error if the insert
// fails — callers should treat that as a fatal enqueue failure, since the
// event would otherwise be lost. The current fireEvent call sites ignore
// the error because under the pre-outbox scheme nothing could be done;
// tightening that is a follow-up once the outbox becomes the default.
func (d *OutboxDispatcher) Dispatch(ctx context.Context, tenantID, eventType string, payload map[string]any) error {
	_, err := d.outbox.EnqueueStandalone(ctx, tenantID, eventType, payload)
	if err != nil {
		mw.RecordWebhookOutboxEnqueueFailure(eventType)
	}
	return err
}
