package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
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

// MailboxMessage is the persisted delivery record for one native comment sent
// to an external agent. Delivery and processing are deliberately separate: a
// delivered message is not a completed run.
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

// MailboxClaim is one message leased to this verified daemon. Message.ID is the
// stable host idempotency key; DeliveryToken fences the ack to this lease.
type MailboxClaim struct {
	Message         MailboxMessage  `json:"message"`
	ExternalSession ExternalSession `json:"external_session"`
	DeliveryToken   string          `json:"delivery_token"`
	LeaseExpiresAt  string          `json:"lease_expires_at"`
	Content         string          `json:"content"`
	// WorkspaceID is the selector this claim was requested under. The server
	// resolves it from X-Workspace-ID and does not echo it in the DTO.
	WorkspaceID string `json:"-"`
}

// MailboxAckRequest is the daemon-side delivery outcome for a claim.
type MailboxAckRequest struct {
	DeliveryToken string `json:"delivery_token"`
	Status        string `json:"status"`
	HostReceipt   string `json:"host_receipt,omitempty"`
	Error         string `json:"error,omitempty"`
	// RetryClass lets the server keep a busy session's request warm instead of
	// applying the failure backoff. Older daemons omit it.
	RetryClass string `json:"retry_class,omitempty"`
}

const (
	mailboxAckDelivered = "delivered"
	mailboxAckRetry     = "retry"
	mailboxAckFailed    = "failed"
)

// ClaimMailbox leases up to limit pending messages for this daemon in one
// workspace. The daemon credential is account-scoped, so X-Workspace-ID is the
// selector the server resolves; it does not grant access the daemon owner
// lacks.
func (c *Client) ClaimMailbox(ctx context.Context, workspaceID string, limit int) ([]MailboxClaim, error) {
	var resp struct {
		Messages []MailboxClaim `json:"messages"`
	}
	if err := c.postDaemonMailboxJSON(ctx, workspaceID, "/api/daemon/mailbox/claim", map[string]any{"limit": limit}, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Messages {
		resp.Messages[i].WorkspaceID = workspaceID
	}
	return resp.Messages, nil
}

// AckMailbox reports a claim outcome. delivery_token fences the write to the
// lease that produced it; a stale token is rejected by the server.
func (c *Client) AckMailbox(ctx context.Context, workspaceID, messageID string, ack MailboxAckRequest) (*MailboxMessage, error) {
	var message MailboxMessage
	path := "/api/daemon/mailbox/" + url.PathEscape(messageID) + "/ack"
	if err := c.postDaemonMailboxJSON(ctx, workspaceID, path, ack, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

// postDaemonMailboxJSON is the daemon client's workspace-scoped POST. Mailbox
// routes resolve the workspace from X-Workspace-ID before checking daemon-owner
// membership, so the header is part of the request contract rather than an
// override.
func (c *Client) postDaemonMailboxJSON(ctx context.Context, workspaceID, path string, reqBody, respBody any) error {
	headers := http.Header{}
	if workspaceID != "" {
		headers.Set("X-Workspace-ID", workspaceID)
	}
	if c.mailboxDaemonID != "" {
		headers.Set("X-Multica-Daemon-ID", c.mailboxDaemonID)
	}
	return c.postJSONWithHeaders(ctx, path, headers, reqBody, respBody)
}

func mailboxLeaseExpired(raw string, now time.Time) (bool, error) {
	lease, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false, fmt.Errorf("invalid mailbox lease_expires_at %q: %w", raw, err)
	}
	return !lease.After(now), nil
}
