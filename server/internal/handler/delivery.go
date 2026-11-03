package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/delivery"
)

// daemonDeliveryClaim is the one claim projection for every source type. The
// worker renders the notification from kind+payload and the stable references
// below: a comment delivery carries its issue/comment ids, a generic event may
// carry only an issue id or none at all. workspace_id comes from the stored
// delivery row, not the claiming request: the frozen envelope must describe the
// row the lease actually covers.
type daemonDeliveryClaim struct {
	ID               string                `json:"id"`
	WorkspaceID      string                `json:"workspace_id"`
	Kind             string                `json:"kind"`
	Payload          json.RawMessage       `json:"payload"`
	SourceType       string                `json:"source_type"`
	IssueID          string                `json:"issue_id,omitempty"`
	CommentID        string                `json:"comment_id,omitempty"`
	RecipientAgentID string                `json:"recipient_agent_id"`
	RecipientType    string                `json:"recipient_type"`
	TargetSessionID  string                `json:"target_session_id"`
	ExternalSession  daemonDeliverySession `json:"external_session"`
	DeliveryStatus   string                `json:"delivery_status"`
	Attempts         int32                 `json:"attempts"`
	VerifyOnly       bool                  `json:"verify_only"`
	DeliveryToken    string                `json:"delivery_token"`
	LeaseExpiresAt   string                `json:"lease_expires_at"`
}

type claimDaemonDeliveriesRequest struct {
	Limit int `json:"limit"`
}

type daemonDeliverySession struct {
	Provider  string `json:"provider"`
	HostID    string `json:"host_id"`
	SessionID string `json:"session_id"`
	DaemonID  string `json:"daemon_id"`
}

// ClaimDaemonDeliveries leases every deliverable source for the verified
// daemon. Comments and generic events share one claim and one outcome state
// machine; there is no second consumer that could lease the same row.
func (h *Handler) ClaimDaemonDeliveries(w http.ResponseWriter, r *http.Request) {
	workspaceID, wsUUID, daemonID, ownerID, ok := h.daemonMailboxContext(w, r)
	if !ok {
		return
	}
	var req claimDaemonDeliveriesRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = daemonDeliveryClaimDefault
	}
	limit = min(limit, daemonDeliveryClaimMax)

	claims, err := delivery.NewQueue(h.Queries, h.TxStarter).Claim(r.Context(), delivery.ClaimRequest{
		Scope:       delivery.Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Limit:       limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim deliveries")
		return
	}
	items := make([]daemonDeliveryClaim, 0, len(claims))
	for _, claim := range claims {
		items = append(items, daemonDeliveryClaim{
			ID:               claim.ID,
			WorkspaceID:      claim.Scope.WorkspaceID,
			Kind:             claim.Kind,
			Payload:          claim.Payload,
			SourceType:       claim.Source.Type,
			IssueID:          claim.Refs.IssueID,
			CommentID:        claim.Refs.CommentID,
			RecipientAgentID: claim.Recipient.ID,
			RecipientType:    claim.Recipient.Type,
			TargetSessionID:  claim.Delivery.Target.SessionID,
			ExternalSession: daemonDeliverySession{
				Provider:  claim.Session.Provider,
				HostID:    claim.Session.HostID,
				SessionID: claim.Session.SessionID,
				DaemonID:  claim.Session.DaemonID,
			},
			DeliveryStatus: claim.DeliveryStatus,
			Attempts:       claim.Attempts,
			VerifyOnly:     claim.VerifyOnly,
			DeliveryToken:  claim.LeaseToken,
			LeaseExpiresAt: claim.LeaseExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	// The issue detail shows per-message delivery state, so a lease is a
	// visible transition there just like an outcome.
	issues := make(map[string]struct{})
	for _, claim := range claims {
		if claim.Refs.IssueID != "" {
			issues[claim.Refs.IssueID] = struct{}{}
		}
	}
	for issueID := range issues {
		h.publishMailboxChanged(wsUUID, issueID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": items})
}

type recordDaemonDeliveryOutcomeRequest struct {
	DeliveryToken string `json:"delivery_token"`
	Outcome       string `json:"outcome"`
	RetryClass    string `json:"retry_class,omitempty"`
	HostReceipt   string `json:"host_receipt,omitempty"`
	Error         string `json:"error,omitempty"`
}

type daemonDeliveryOutcome struct {
	ID               string  `json:"id"`
	DeliveryStatus   string  `json:"delivery_status"`
	HostReceipt      *string `json:"host_receipt"`
	LastError        *string `json:"last_error"`
	DeliveredAt      *string `json:"delivered_at"`
	ProcessingStatus string  `json:"processing_status"`
}

// RecordDaemonDeliveryOutcome stores the worker's fenced result. It is the
// generic counterpart of the mailbox ack: the token must match the lease that
// produced the claim, and a stale token is refused rather than overwriting a
// later lease.
func (h *Handler) RecordDaemonDeliveryOutcome(w http.ResponseWriter, r *http.Request) {
	workspaceID, wsUUID, daemonID, _, ok := h.daemonMailboxContext(w, r)
	if !ok {
		return
	}
	deliveryID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "deliveryId"), "delivery id")
	if !ok {
		return
	}
	var req recordDaemonDeliveryOutcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DeliveryToken == "" {
		writeError(w, http.StatusBadRequest, "delivery_token is required")
		return
	}
	state, ok := deliveryOutcomeState(req.Outcome)
	if !ok {
		writeError(w, http.StatusBadRequest, "outcome must be 'delivered', 'retry', 'rejected' or 'unknown'")
		return
	}
	if req.Outcome == string(delivery.OutcomeUnknown) && req.Error == "" {
		writeError(w, http.StatusBadRequest, "an unknown outcome requires an error diagnostic")
		return
	}
	if state == delivery.OutcomeDelivered && req.Error != "" {
		writeError(w, http.StatusBadRequest, "a delivered outcome cannot carry an error")
		return
	}
	if !validRetryClass(req.RetryClass) {
		writeError(w, http.StatusBadRequest, "retry_class must be 'busy', 'unavailable' or 'dependency'")
		return
	}
	if req.RetryClass != "" && req.Outcome != string(delivery.OutcomeRetry) {
		writeError(w, http.StatusBadRequest, "retry_class is only valid for a retry outcome")
		return
	}
	stored, err := delivery.NewQueue(h.Queries, h.TxStarter).RecordOutcome(r.Context(), delivery.OutcomeRequest{
		Scope:       delivery.Scope{WorkspaceID: workspaceID},
		DeliveryID:  uuidToString(deliveryID),
		DaemonID:    daemonID,
		LeaseToken:  req.DeliveryToken,
		State:       state,
		RetryClass:  delivery.RetryClass(req.RetryClass),
		HostReceipt: req.HostReceipt,
		Error:       req.Error,
	})
	if err != nil {
		if errors.Is(err, delivery.ErrLeaseLost) {
			writeError(w, http.StatusConflict, "delivery lease is stale or does not belong to this daemon")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to record delivery outcome")
		return
	}
	if stored.Refs.IssueID != "" {
		h.publishMailboxChanged(wsUUID, stored.Refs.IssueID)
	}
	writeJSON(w, http.StatusOK, daemonDeliveryOutcome{
		ID:               stored.ID,
		DeliveryStatus:   stored.DeliveryStatus,
		HostReceipt:      stringPtr(stored.HostReceipt),
		LastError:        stringPtr(stored.LastError),
		DeliveredAt:      timePtr(stored.DeliveredAt),
		ProcessingStatus: stored.ProcessingStatus,
	})
}

func deliveryOutcomeState(outcome string) (delivery.OutcomeState, bool) {
	switch outcome {
	case string(delivery.OutcomeDelivered):
		return delivery.OutcomeDelivered, true
	case string(delivery.OutcomeRetry):
		return delivery.OutcomeRetry, true
	case string(delivery.OutcomeRejected):
		return delivery.OutcomeRejected, true
	case string(delivery.OutcomeUnknown):
		return delivery.OutcomeUnknown, true
	default:
		return "", false
	}
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func timePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}
