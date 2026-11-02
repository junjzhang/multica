package daemon

import (
	"context"
	"net/http"
)

// ExternalSession identifies one session in an external agent host. HostID is
// the host's own server/connection id (not a hostname); SessionID is the
// host-side agent id (for Paseo, the existing Paseo agent id).
type ExternalSession struct {
	ID          string  `json:"id,omitempty"`
	Provider    string  `json:"provider"`
	HostID      string  `json:"host_id"`
	SessionID   string  `json:"session_id"`
	DisplayName string  `json:"display_name"`
	URL         *string `json:"url"`
	DaemonID    string  `json:"daemon_id"`
}

// MailboxMessage is the user-side mailbox record the CLI lists. Delivery and
// processing are deliberately separate: a delivered message is not a completed
// run. Claiming for delivery is the delivery worker's contract, not this DTO.
type MailboxMessage struct {
	ID               string           `json:"id"`
	IssueID          string           `json:"issue_id"`
	CommentID        string           `json:"comment_id"`
	RecipientAgentID string           `json:"recipient_agent_id"`
	SourceSession    *ExternalSession `json:"source_session"`
	DeliveryStatus   string           `json:"delivery_status"`
	ProcessingStatus string           `json:"processing_status"`
	CreatedAt        string           `json:"created_at"`
	DeliveredAt      *string          `json:"delivered_at"`
	LastError        *string          `json:"last_error"`
}

// postDaemonDeliveryJSON is the daemon client's workspace-scoped POST. Delivery
// routes resolve the workspace from X-Workspace-ID before checking daemon-owner
// membership, so the header is part of the request contract rather than an
// override.
func (c *Client) postDaemonDeliveryJSON(ctx context.Context, workspaceID, path string, reqBody, respBody any) error {
	headers := http.Header{}
	if workspaceID != "" {
		headers.Set("X-Workspace-ID", workspaceID)
	}
	if c.mailboxDaemonID != "" {
		headers.Set("X-Multica-Daemon-ID", c.mailboxDaemonID)
	}
	return c.postJSONWithHeaders(ctx, path, headers, reqBody, respBody)
}
