package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/multica-ai/multica/server/internal/delivery"
)

// deliveryClaimDTO mirrors the generic claim response. It is deliberately
// separate from MailboxClaim: a generic delivery has no issue or comment, and
// the legacy DTO must keep rendering an issue-shaped notification.
type deliveryClaimDTO struct {
	ID               string          `json:"id"`
	Kind             string          `json:"kind"`
	Payload          json.RawMessage `json:"payload"`
	SourceType       string          `json:"source_type"`
	IssueID          string          `json:"issue_id"`
	CommentID        string          `json:"comment_id"`
	RecipientAgentID string          `json:"recipient_agent_id"`
	RecipientType    string          `json:"recipient_type"`
	ExternalSession  struct {
		Provider  string `json:"provider"`
		HostID    string `json:"host_id"`
		SessionID string `json:"session_id"`
		DaemonID  string `json:"daemon_id"`
	} `json:"external_session"`
	DeliveryStatus string `json:"delivery_status"`
	Attempts       int32  `json:"attempts"`
	VerifyOnly     bool   `json:"verify_only"`
	DeliveryToken  string `json:"delivery_token"`
	LeaseExpiresAt string `json:"lease_expires_at"`
}

type deliveryOutcomeDTO struct {
	ID               string  `json:"id"`
	DeliveryStatus   string  `json:"delivery_status"`
	HostReceipt      *string `json:"host_receipt"`
	LastError        *string `json:"last_error"`
	DeliveredAt      *string `json:"delivered_at"`
	ProcessingStatus string  `json:"processing_status"`
}

// mailboxStatus extracts the HTTP status from the daemon client's request
// error so claim/outcome failures can be classified without string matching.
func mailboxStatus(err error) int {
	var reqErr *requestError
	if !errors.As(err, &reqErr) {
		return 0
	}
	return reqErr.StatusCode
}

// ClaimDeliveries leases every deliverable source for this daemon: comment
// notifications and generic events arrive through the same claim and outcome
// contract.
func (c *Client) ClaimDeliveries(ctx context.Context, workspaceID string, limit int) ([]delivery.Claim, error) {
	var resp struct {
		Deliveries []deliveryClaimDTO `json:"deliveries"`
	}
	if err := c.postDaemonDeliveryJSON(ctx, workspaceID, "/api/daemon/deliveries/claim", map[string]any{"limit": limit}, &resp); err != nil {
		if mailboxStatus(err) == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %v", delivery.ErrNotFound, err)
		}
		return nil, err
	}
	claims := make([]delivery.Claim, 0, len(resp.Deliveries))
	for _, item := range resp.Deliveries {
		recipientType := item.RecipientType
		if recipientType == "" {
			recipientType = "agent"
		}
		recipient := delivery.Recipient{Type: recipientType, ID: item.RecipientAgentID}
		claim := delivery.Claim{
			Delivery: delivery.Delivery{
				ID:             item.ID,
				Scope:          delivery.Scope{WorkspaceID: workspaceID},
				Source:         delivery.Source{Type: item.SourceType},
				Recipient:      recipient,
				Kind:           item.Kind,
				Payload:        item.Payload,
				Refs:           delivery.Refs{IssueID: item.IssueID, CommentID: item.CommentID},
				DeliveryStatus: item.DeliveryStatus,
				Attempts:       item.Attempts,
			},
			Session: delivery.HostTarget{
				Recipient: recipient,
				Provider:  item.ExternalSession.Provider,
				HostID:    item.ExternalSession.HostID,
				SessionID: item.ExternalSession.SessionID,
				DaemonID:  item.ExternalSession.DaemonID,
			},
			LeaseToken: item.DeliveryToken,
			VerifyOnly: item.VerifyOnly,
		}
		leaseExpiresAt, err := time.Parse(time.RFC3339, item.LeaseExpiresAt)
		if err != nil {
			// Skip only this lease instead of discarding the whole batch. The
			// worker records a retry for it: the host was never called, and
			// leaving the row leased would later turn a never-sent delivery
			// into an unverifiable unknown.
			claim.ClaimError = fmt.Sprintf("delivery claim %s has an invalid lease_expires_at %q: %v", item.ID, item.LeaseExpiresAt, err)
			claims = append(claims, claim)
			continue
		}
		claim.LeaseExpiresAt = leaseExpiresAt
		claims = append(claims, claim)
	}
	return claims, nil
}

// RecordDeliveryOutcome stores one fenced worker result. A stale lease is
// reported as delivery.ErrLeaseLost so the worker waits for the next claim
// instead of dropping a decided outcome.
func (c *Client) RecordDeliveryOutcome(ctx context.Context, workspaceID string, req delivery.OutcomeRequest) (delivery.Delivery, error) {
	body := map[string]any{
		"delivery_token": req.LeaseToken,
		"outcome":        string(req.State),
		"host_receipt":   req.HostReceipt,
		"error":          req.Error,
	}
	if req.RetryClass != "" {
		body["retry_class"] = string(req.RetryClass)
	}
	path := "/api/daemon/deliveries/" + url.PathEscape(req.DeliveryID) + "/outcome"
	var resp deliveryOutcomeDTO
	if err := c.postDaemonDeliveryJSON(ctx, workspaceID, path, body, &resp); err != nil {
		switch mailboxStatus(err) {
		case http.StatusConflict:
			return delivery.Delivery{}, fmt.Errorf("%w: %v", delivery.ErrLeaseLost, err)
		case http.StatusNotFound:
			return delivery.Delivery{}, fmt.Errorf("%w: %v", delivery.ErrNotFound, err)
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
			return delivery.Delivery{}, fmt.Errorf("%w: %v", delivery.ErrOutcomeRejected, err)
		default:
			return delivery.Delivery{}, err
		}
	}
	stored := delivery.Delivery{
		ID:               resp.ID,
		Scope:            req.Scope,
		DeliveryStatus:   resp.DeliveryStatus,
		ProcessingStatus: resp.ProcessingStatus,
	}
	if resp.HostReceipt != nil {
		stored.HostReceipt = *resp.HostReceipt
	}
	if resp.LastError != nil {
		stored.LastError = *resp.LastError
	}
	if resp.DeliveredAt != nil {
		if parsed, err := time.Parse(time.RFC3339, *resp.DeliveredAt); err == nil {
			stored.DeliveredAt = &parsed
		}
	}
	return stored, nil
}

var _ delivery.OutcomeClient = (*Client)(nil)
