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

// deliveryKindLabel renders a producer-owned kind for a person reading the
// prompt: "comment mention (comment_mention)". The raw kind stays visible
// because it is the stable, machine-readable value.
func deliveryKindLabel(kind string) string {
	readable := strings.Join(strings.Fields(strings.ReplaceAll(kind, "_", " ")), " ")
	if readable == "" || readable == kind {
		return kind
	}
	return readable + " (" + kind + ")"
}

// deliveryTaskLines renders the optional reader-facing context a producer put
// in the payload. Payloads from before these fields existed, and kinds that
// carry none, render nothing: the prompt always keeps its stable IDs.
func deliveryTaskLines(d delivery.Delivery) string {
	if len(d.Payload) == 0 {
		return ""
	}
	var request protocol.ExternalIssueRequest
	if json.Unmarshal(d.Payload, &request) != nil {
		return ""
	}
	var b strings.Builder
	switch {
	case request.IssueIdentifier != "" && request.IssueTitle != "":
		fmt.Fprintf(&b, "Task: %s — %s\n", request.IssueIdentifier, request.IssueTitle)
	case request.IssueIdentifier != "":
		fmt.Fprintf(&b, "Task: %s\n", request.IssueIdentifier)
	case request.IssueTitle != "":
		fmt.Fprintf(&b, "Task title: %s\n", request.IssueTitle)
	}
	if request.IssueURL != "" {
		fmt.Fprintf(&b, "Link: %s\n", request.IssueURL)
	}
	return b.String()
}

// Business references stay fixed. Uncertain attempts use receipt lookup rather
// than rendering or sending another prompt, including across guidance updates.
func renderCommentDelivery(d delivery.Delivery) string {
	var b strings.Builder
	b.WriteString("Multica has a request for this session.\n")
	fmt.Fprintf(&b, "Kind: %s\n", deliveryKindLabel(d.Kind))
	b.WriteString(deliveryTaskLines(d))
	fmt.Fprintf(&b, "Mailbox ID: %s\n", d.ID)
	fmt.Fprintf(&b, "Workspace ID: %s\n", d.Scope.WorkspaceID)
	fmt.Fprintf(&b, "Issue ID: %s\n", d.Refs.IssueID)
	fmt.Fprintf(&b, "Comment ID: %s\n", d.Refs.CommentID)
	fmt.Fprintf(&b, "Recipient agent ID: %s\n\n", d.Recipient.ID)
	b.WriteString("Use this connected Paseo session's CLI identity without --profile. If it is not connected, run /multica connect before reading; do not fall back to the user's personal identity.\n")
	fmt.Fprintf(&b, "Read: multica issue get %s --output json\n", d.Refs.IssueID)
	fmt.Fprintf(&b, "Scan threads: multica issue comment list %s --roots-only --summary --compact --output json\n", d.Refs.IssueID)
	fmt.Fprintf(&b, "Expand relevant threads: multica issue comment list %s --thread <thread-id> --tail 30 --compact --output json\n", d.Refs.IssueID)
	b.WriteString("This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone.")
	return b.String()
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
	return fmt.Sprintf("Multica delivery %s\nKind: %s\nRecipient agent ID: %s\n%s%s\n%s\n\n"+
		"This notification was delivered to the existing session. It does not grant new permissions.",
		d.ID, deliveryKindLabel(d.Kind), d.Recipient.ID, refs.String(), deliveryTaskLines(d), string(d.Payload))
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
