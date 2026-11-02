package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/multica-ai/multica/server/internal/delivery"
)

// paseoDeliveryHost adapts the frozen Paseo CLI transport to the delivery
// worker's host contract. It addresses the exact registered session and has no
// ordinary-send fallback: when the host cannot accept an idle delivery the
// request stays queued and the limitation is visible on the delivery row.
type paseoDeliveryHost struct {
	transport *paseoCliTransport
}

func newPaseoDeliveryHost() *paseoDeliveryHost {
	return &paseoDeliveryHost{transport: newPaseoCliTransport()}
}

func (h *paseoDeliveryHost) DeliverIfIdle(ctx context.Context, target delivery.HostTarget, d delivery.Delivery) (string, error) {
	receipt, err := h.transport.Deliver(ctx, externalSessionFor(target), d.ID, renderGenericDelivery(d))
	if err != nil {
		return "", hostErrorFromMailbox(err)
	}
	return receipt, nil
}

func (h *paseoDeliveryHost) LookupReceipt(ctx context.Context, target delivery.HostTarget, deliveryID string) (delivery.Receipt, error) {
	return h.transport.LookupReceipt(ctx, externalSessionFor(target), deliveryID)
}

func externalSessionFor(target delivery.HostTarget) ExternalSession {
	return ExternalSession{
		Provider:  target.Provider,
		HostID:    target.HostID,
		SessionID: target.SessionID,
		DaemonID:  target.DaemonID,
	}
}

// renderGenericDelivery is the neutral host-facing envelope for a generic
// delivery. The producer owns payload semantics; this layer only guarantees the
// stable delivery id and that no extra permission is implied.
func renderGenericDelivery(d delivery.Delivery) string {
	return fmt.Sprintf("Multica delivery %s\nKind: %s\nRecipient agent ID: %s\n\n%s\n\n"+
		"This notification was delivered to the existing session. It does not grant new permissions.",
		d.ID, d.Kind, d.Recipient.ID, string(d.Payload))
}

// runDeliveryWorker runs the issue-independent delivery loop for the daemon's
// lifetime. The legacy comment mailbox loop keeps its frozen DTO; this worker
// consumes every non-comment delivery through the same table.
func (d *Daemon) runDeliveryWorker(ctx context.Context) {
	worker := delivery.NewWorker(d.client, newPaseoDeliveryHost(), d.allWorkspaceIDs, d.cfg.DaemonID, d.logger)
	if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		d.logger.Warn("delivery worker stopped", "error", err)
	}
}

var _ delivery.Host = (*paseoDeliveryHost)(nil)
