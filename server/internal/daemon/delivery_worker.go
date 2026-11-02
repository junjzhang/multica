package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/delivery"
)

// commentDeliveryKind is the producer-owned kind for an issue/comment
// notification. It selects the frozen comment envelope below.
const commentDeliveryKind = "comment_mention"

// paseoDeliveryHost adapts the frozen Paseo CLI transport to the delivery
// worker's host contract. It addresses the exact registered session and has no
// ordinary-send fallback: when the host cannot accept an idle delivery the
// request stays queued and the limitation is visible on the delivery row. The
// same adapter serves comment notifications and generic events.
type paseoDeliveryHost struct {
	transport *paseoCliTransport
}

func newPaseoDeliveryHost() *paseoDeliveryHost {
	return &paseoDeliveryHost{transport: newPaseoCliTransport()}
}

func (h *paseoDeliveryHost) DeliverIfIdle(ctx context.Context, target delivery.HostTarget, d delivery.Delivery) (string, error) {
	receipt, err := h.transport.Deliver(ctx, externalSessionFor(target), d.ID, renderDeliveryNotification(d))
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

// Comment deliveries keep the exact envelope the frozen mailbox consumer sent.
// The host deduplicates by delivery id, so changing this text for an in-flight
// row would change its idempotency fingerprint.
func renderCommentDelivery(d delivery.Delivery) string {
	return fmt.Sprintf("Multica has a request for this session.\nMailbox ID: %s\nWorkspace ID: %s\nIssue ID: %s\nComment ID: %s\nRecipient agent ID: %s\n\n"+
		"Use the external-agent CLI profile provisioned when this session was registered. If it is unavailable, report that dependency; do not fall back to the user's personal identity.\n"+
		"Read: multica --profile <registered-external-profile> issue get %[3]s --output json\n"+
		"Read comments: multica --profile <registered-external-profile> issue comment list %[3]s --output json\n"+
		"This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone.",
		d.ID, d.Scope.WorkspaceID, d.Refs.IssueID, d.Refs.CommentID, d.Recipient.ID)
}

// renderDeliveryNotification is the one host-facing envelope. A comment
// notification keeps its frozen text; every other kind is a neutral envelope
// carrying the stable delivery id and any issue reference. The producer owns
// payload semantics; this layer only guarantees stable references and that no
// extra permission is implied.
func renderDeliveryNotification(d delivery.Delivery) string {
	if d.Kind == commentDeliveryKind {
		return renderCommentDelivery(d)
	}
	var refs strings.Builder
	if d.Refs.IssueID != "" {
		fmt.Fprintf(&refs, "Issue ID: %s\n", d.Refs.IssueID)
	}
	return fmt.Sprintf("Multica delivery %s\nKind: %s\nRecipient agent ID: %s\n%s\n%s\n\n"+
		"This notification was delivered to the existing session. It does not grant new permissions.",
		d.ID, d.Kind, d.Recipient.ID, refs.String(), string(d.Payload))
}

// runDeliveryWorker runs the one delivery loop for the daemon's lifetime. Every
// source is consumed here: comments and generic events share the table, the
// lease and the receipt-recovery path.
func (d *Daemon) runDeliveryWorker(ctx context.Context) {
	worker := delivery.NewWorker(d.client, newPaseoDeliveryHost(), d.allWorkspaceIDs, d.cfg.DaemonID, d.logger)
	if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		d.logger.Warn("delivery worker stopped", "error", err)
	}
}

var _ delivery.Host = (*paseoDeliveryHost)(nil)
