package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/pkg/protocol"
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
		return "", err
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

// Business references stay fixed. Uncertain attempts use receipt lookup rather
// than rendering or sending another prompt, including across guidance updates.
func renderCommentDelivery(d delivery.Delivery) string {
	return fmt.Sprintf("Multica has a request for this session.\nMailbox ID: %s\nWorkspace ID: %s\nIssue ID: %s\nComment ID: %s\nRecipient agent ID: %s\n\n"+
		"Use this connected Paseo session's CLI identity without --profile. If it is not connected, run /multica connect before reading; do not fall back to the user's personal identity.\n"+
		"Read: multica issue get %[3]s --output json\n"+
		"Scan threads: multica issue comment list %[3]s --roots-only --summary --compact --output json\n"+
		"Expand relevant threads: multica issue comment list %[3]s --thread <thread-id> --tail 30 --compact --output json\n"+
		"This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone.",
		d.ID, d.Scope.WorkspaceID, d.Refs.IssueID, d.Refs.CommentID, d.Recipient.ID)
}

// renderDeliveryNotification is the one host-facing envelope. A comment
// notification carries its fixed references; every other kind is a neutral envelope
// carrying the stable delivery id and any issue reference. The producer owns
// payload semantics; this layer only guarantees stable references and that no
// extra permission is implied.
func renderDeliveryNotification(d delivery.Delivery) string {
	if d.Kind == commentDeliveryKind || d.Kind == "issue_assignment" {
		body := renderCommentDelivery(d)
		var request protocol.ExternalIssueRequest
		if json.Unmarshal(d.Payload, &request) == nil && request.SquadID != "" && request.Briefing != "" {
			body += fmt.Sprintf("\n\nSquad ID: %s\nFor this external turn, append --mailbox-id %s to multica squad activity. Use the connected external identity; do not set X-Task-ID or fabricate a managed run.\n\n%s", request.SquadID, d.ID, request.Briefing)
		}
		return body
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
