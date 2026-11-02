package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// MailboxMessageResponse is the frozen mailbox DTO. The body and reply
// relationship stay in the native comment; this carries recipient + delivery
// and processing receipts only. Delivered is not handled: receiving a request
// never means the external agent started or finished it.
type MailboxMessageResponse struct {
	ID               string                   `json:"id"`
	IssueID          string                   `json:"issue_id"`
	CommentID        string                   `json:"comment_id"`
	RecipientAgentID string                   `json:"recipient_agent_id"`
	SourceSession    *ExternalSessionResponse `json:"source_session"`
	DeliveryStatus   string                   `json:"delivery_status"`
	ProcessingStatus string                   `json:"processing_status"`
	CreatedAt        string                   `json:"created_at"`
	DeliveredAt      *string                  `json:"delivered_at"`
	LastError        *string                  `json:"last_error"`
}

func mailboxMessageToResponse(m db.AgentMailboxMessage, source *ExternalSessionResponse) MailboxMessageResponse {
	return MailboxMessageResponse{
		ID:               uuidToString(m.ID),
		IssueID:          uuidToString(m.IssueID),
		CommentID:        uuidToString(m.CommentID),
		RecipientAgentID: uuidToString(m.RecipientAgentID),
		SourceSession:    source,
		DeliveryStatus:   m.DeliveryStatus,
		ProcessingStatus: m.ProcessingStatus,
		CreatedAt:        timestampToString(m.CreatedAt),
		DeliveredAt:      timestampToPtr(m.DeliveredAt),
		LastError:        textToPtr(m.LastError),
	}
}

const (
	mailboxLimitDefault = 50
	mailboxLimitMax     = 100

	daemonMailboxClaimDefault = 10
	daemonMailboxClaimMax     = 50
)

// encodeMailboxCursor renders the opaque keyset cursor (created_at, id).
func encodeMailboxCursor(createdAt pgtype.Timestamptz, id pgtype.UUID) string {
	raw := createdAt.Time.UTC().Format(time.RFC3339Nano) + "|" + uuidToString(id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeMailboxCursor(cursor string) (pgtype.Timestamptz, pgtype.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid cursor")
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid cursor")
	}
	id, err := util.ParseUUID(parts[1])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid cursor")
	}
	return pgtype.Timestamptz{Time: createdAt, Valid: true}, id, nil
}

// loadMailboxAgentForActor resolves {id} to an external agent the caller may
// inspect: the agent itself presenting its own external credential, or a human
// owner/admin. Anyone else is refused.
func (h *Handler) loadMailboxAgentForActor(w http.ResponseWriter, r *http.Request) (db.Agent, bool) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return db.Agent{}, false
	}
	agentID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "agent id")
	if !ok {
		return db.Agent{}, false
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentID,
		WorkspaceID: wsUUID,
	})
	if err != nil || agent.ExecutionKind != "external" {
		writeError(w, http.StatusNotFound, "external agent not found")
		return db.Agent{}, false
	}
	if r.Header.Get("X-Actor-Source") == "external_agent" {
		if r.Header.Get("X-Agent-ID") != uuidToString(agent.ID) {
			writeError(w, http.StatusForbidden, "mailbox credentials may only read their own mailbox")
			return db.Agent{}, false
		}
		return agent, true
	}
	if !h.canManageAgent(w, r, agent) {
		return db.Agent{}, false
	}
	return agent, true
}

// ListAgentMailbox returns one keyset page of the recipient's mailbox. Reading
// never deletes and never moves processing state.
func (h *Handler) ListAgentMailbox(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadMailboxAgentForActor(w, r)
	if !ok {
		return
	}
	limit := mailboxLimitDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(parsed, mailboxLimitMax)
	}
	var afterCreated pgtype.Timestamptz
	var afterID pgtype.UUID
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		afterCreated, afterID, err = decodeMailboxCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	rows, err := h.Queries.ListAgentMailboxMessages(r.Context(), db.ListAgentMailboxMessagesParams{
		RecipientAgentID: agent.ID,
		WorkspaceID:      agent.WorkspaceID,
		AfterCreatedAt:   afterCreated,
		AfterID:          afterID,
		PageLimit:        int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mailbox")
		return
	}
	sources := h.externalSessionsByID(r.Context(), mailboxSourceSessionIDs(rows))
	messages := make([]MailboxMessageResponse, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, mailboxMessageToResponse(row, sources[uuidToString(row.SourceSessionID)]))
	}
	var nextCursor *string
	if len(rows) == limit {
		last := rows[len(rows)-1]
		cursor := encodeMailboxCursor(last.CreatedAt, last.ID)
		nextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":    messages,
		"next_cursor": nextCursor,
	})
}

func mailboxSourceSessionIDs(rows []db.AgentMailboxMessage) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		if row.SourceSessionID.Valid {
			out = append(out, row.SourceSessionID)
		}
	}
	return out
}

func (h *Handler) externalSessionsByID(ctx context.Context, ids []pgtype.UUID) map[string]*ExternalSessionResponse {
	out := map[string]*ExternalSessionResponse{}
	if len(ids) == 0 {
		return out
	}
	rows, err := h.Queries.ListExternalAgentSessionsByIDs(ctx, ids)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[uuidToString(row.ID)] = externalSessionToResponse(row)
	}
	return out
}

type ackMailboxProcessingRequest struct {
	State string `json:"state"`
}

// AckAgentMailbox records read/handled progress. The transition is monotonic:
// `read` can never undo `handled`, and repeated acks are idempotent.
func (h *Handler) AckAgentMailbox(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadMailboxAgentForActor(w, r)
	if !ok {
		return
	}
	var req ackMailboxProcessingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.State != "read" && req.State != "handled" {
		writeError(w, http.StatusBadRequest, "state must be 'read' or 'handled'")
		return
	}
	messageID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "messageId"), "message id")
	if !ok {
		return
	}
	updated, err := h.Queries.AckAgentMailboxProcessing(r.Context(), db.AckAgentMailboxProcessingParams{
		ID:               messageID,
		WorkspaceID:      agent.WorkspaceID,
		RecipientAgentID: agent.ID,
		State:            req.State,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "mailbox message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to acknowledge mailbox message")
		return
	}
	h.publishMailboxChanged(updated.WorkspaceID, uuidToString(updated.IssueID))
	sources := h.externalSessionsByID(r.Context(), mailboxSourceSessionIDs([]db.AgentMailboxMessage{updated}))
	writeJSON(w, http.StatusOK, mailboxMessageToResponse(updated, sources[uuidToString(updated.SourceSessionID)]))
}

// daemonMailboxContext resolves the verified daemon identity for claim/ack.
// Device tokens carry the daemon identity. User credentials require a selector
// whose ownership is checked in the selected workspace.
func (h *Handler) daemonMailboxContext(w http.ResponseWriter, r *http.Request) (workspaceID string, wsUUID pgtype.UUID, daemonID string, ownerID string, ok bool) {
	if r.Header.Get("X-Actor-Source") == "external_agent" || strings.HasPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), auth.ExternalAgentTokenPrefix) {
		writeError(w, http.StatusForbidden, "external agent credentials cannot use daemon mailbox endpoints")
		return "", pgtype.UUID{}, "", "", false
	}
	workspaceID = middleware.DaemonWorkspaceIDFromContext(r.Context())
	if workspaceID == "" {
		workspaceID = h.resolveWorkspaceID(r)
	}
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return "", pgtype.UUID{}, "", "", false
	}
	wsUUID, parsed := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !parsed {
		return "", pgtype.UUID{}, "", "", false
	}
	workspaceID = uuidToString(wsUUID)
	daemonID = middleware.DaemonIDFromContext(r.Context())
	ownerID = middleware.DaemonTokenOwnerIDFromContext(r.Context())
	if ownerID == "" {
		ownerID = requestUserID(r)
	}
	if ownerID == "" {
		writeError(w, http.StatusForbidden, "daemon credential has no human owner")
		return "", pgtype.UUID{}, "", "", false
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      parseUUID(ownerID),
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusForbidden, "daemon owner is not a member of this workspace")
		return "", pgtype.UUID{}, "", "", false
	}
	selector := strings.TrimSpace(r.Header.Get("X-Multica-Daemon-ID"))
	if daemonID != "" && selector != "" && selector != daemonID {
		writeError(w, http.StatusForbidden, "daemon selector does not match the credential")
		return "", pgtype.UUID{}, "", "", false
	}
	if daemonID == "" {
		if selector == "" {
			writeError(w, http.StatusBadRequest, "X-Multica-Daemon-ID is required with user credentials")
			return "", pgtype.UUID{}, "", "", false
		}
		owned, err := h.Queries.WorkspaceDaemonOwnedByUser(r.Context(), db.WorkspaceDaemonOwnedByUserParams{
			WorkspaceID: wsUUID, DaemonID: pgtype.Text{String: selector, Valid: true}, OwnerUserID: parseUUID(ownerID),
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to verify daemon ownership")
			return "", pgtype.UUID{}, "", "", false
		}
		if !owned {
			writeError(w, http.StatusForbidden, "daemon is not owned by this user in this workspace")
			return "", pgtype.UUID{}, "", "", false
		}
		daemonID = selector
	}
	return workspaceID, wsUUID, daemonID, ownerID, true
}

type claimDaemonMailboxRequest struct {
	Limit int `json:"limit"`
}

type daemonMailboxClaimItem struct {
	Message         MailboxMessageResponse  `json:"message"`
	ExternalSession ExternalSessionResponse `json:"external_session"`
	DeliveryToken   string                  `json:"delivery_token"`
	LeaseExpiresAt  string                  `json:"lease_expires_at"`
	Content         string                  `json:"content"`
}

// ClaimDaemonMailbox leases deliverable requests for the verified daemon and
// returns a rendered notification. The claim is fenced: each message gets a
// fresh delivery token, stored hashed, and only the daemon that holds the lease
// can acknowledge it.
func (h *Handler) ClaimDaemonMailbox(w http.ResponseWriter, r *http.Request) {
	workspaceID, wsUUID, daemonID, ownerID, ok := h.daemonMailboxContext(w, r)
	if !ok {
		return
	}
	var req claimDaemonMailboxRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = daemonMailboxClaimDefault
	}
	limit = min(limit, daemonMailboxClaimMax)

	claims, err := delivery.NewQueue(h.Queries, h.TxStarter).Claim(r.Context(), delivery.ClaimRequest{
		Scope:       delivery.Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Limit:       limit,
		Projection:  delivery.CommentProjection,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim mailbox")
		return
	}
	items := make([]daemonMailboxClaimItem, 0, len(claims))
	for _, claim := range claims {
		leased, err := h.Queries.GetAgentMailboxMessage(r.Context(), db.GetAgentMailboxMessageParams{
			ID:          parseUUID(claim.ID),
			WorkspaceID: wsUUID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load claimed mailbox message")
			return
		}
		// The lease carries the target the producer froze. Loading it by id
		// keeps a rebind from changing where an existing message is delivered.
		session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{
			ID:          parseUUID(claim.Delivery.Target.SessionID),
			WorkspaceID: wsUUID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load external session")
			return
		}
		content := renderMailboxNotification(leased)
		var source *ExternalSessionResponse
		if leased.SourceSessionID.Valid {
			if row, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{
				ID:          leased.SourceSessionID,
				WorkspaceID: wsUUID,
			}); err == nil {
				source = externalSessionToResponse(row)
			}
		}
		items = append(items, daemonMailboxClaimItem{
			Message:         mailboxMessageToResponse(leased, source),
			ExternalSession: *externalSessionToResponse(session),
			DeliveryToken:   claim.LeaseToken,
			LeaseExpiresAt:  timestampToString(pgtype.Timestamptz{Time: claim.LeaseExpiresAt, Valid: true}),
			Content:         content,
		})
	}
	issues := make(map[string]struct{})
	for _, item := range items {
		issues[item.Message.IssueID] = struct{}{}
	}
	for issueID := range issues {
		h.publishMailboxChanged(wsUUID, issueID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": items})
}

// Stable references keep the host's idempotency fingerprint unchanged when
// an issue is renamed or its discussion is edited between delivery attempts.
func renderMailboxNotification(m db.AgentMailboxMessage) string {
	return fmt.Sprintf("Multica has a request for this session.\nMailbox ID: %s\nWorkspace ID: %s\nIssue ID: %s\nComment ID: %s\nRecipient agent ID: %s\n\n"+
		"Use the external-agent CLI profile provisioned when this session was registered. If it is unavailable, report that dependency; do not fall back to the user's personal identity.\n"+
		"Read: multica --profile <registered-external-profile> issue get %[3]s --output json\n"+
		"Read comments: multica --profile <registered-external-profile> issue comment list %[3]s --output json\n"+
		"This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone.",
		uuidToString(m.ID), uuidToString(m.WorkspaceID), uuidToString(m.IssueID), uuidToString(m.CommentID), uuidToString(m.RecipientAgentID))
}

type ackDaemonMailboxRequest struct {
	DeliveryToken string `json:"delivery_token"`
	Status        string `json:"status"`
	HostReceipt   string `json:"host_receipt"`
	Error         string `json:"error"`
	// RetryClass separates a busy session from a broken dependency. Older
	// daemons omit it and keep the failure backoff.
	RetryClass string `json:"retry_class,omitempty"`
}

// AckDaemonMailbox records the daemon's delivery result. The lease token fences
// the write: a stale or foreign ack matches no row and cannot overwrite a later
// lease.
func (h *Handler) AckDaemonMailbox(w http.ResponseWriter, r *http.Request) {
	workspaceID, wsUUID, daemonID, _, ok := h.daemonMailboxContext(w, r)
	if !ok {
		return
	}
	messageID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "messageId"), "message id")
	if !ok {
		return
	}
	var req ackDaemonMailboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DeliveryToken == "" {
		writeError(w, http.StatusBadRequest, "delivery_token is required")
		return
	}
	state, ok := mailboxAckState(req.Status)
	if !ok {
		writeError(w, http.StatusBadRequest, "status must be 'delivered', 'retry' or 'failed'")
		return
	}
	if !validRetryClass(req.RetryClass) {
		writeError(w, http.StatusBadRequest, "retry_class must be 'busy', 'unavailable' or 'dependency'")
		return
	}
	if _, err := h.Queries.GetAgentMailboxMessage(r.Context(), db.GetAgentMailboxMessageParams{
		ID:          messageID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "mailbox message not found")
		return
	}
	// Both device credentials and owner-verified selectors fence the lease.
	if _, err := delivery.NewQueue(h.Queries, h.TxStarter).RecordOutcome(r.Context(), delivery.OutcomeRequest{
		Scope:       delivery.Scope{WorkspaceID: workspaceID},
		DeliveryID:  uuidToString(messageID),
		DaemonID:    daemonID,
		LeaseToken:  req.DeliveryToken,
		State:       state,
		RetryClass:  delivery.RetryClass(req.RetryClass),
		HostReceipt: req.HostReceipt,
		Error:       req.Error,
	}); err != nil {
		if errors.Is(err, delivery.ErrLeaseLost) {
			writeError(w, http.StatusConflict, "delivery lease is stale or does not belong to this daemon")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to acknowledge delivery")
		return
	}
	updated, err := h.Queries.GetAgentMailboxMessage(r.Context(), db.GetAgentMailboxMessageParams{
		ID:          messageID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load acknowledged mailbox message")
		return
	}
	h.publishMailboxChanged(updated.WorkspaceID, uuidToString(updated.IssueID))
	sources := h.externalSessionsByID(r.Context(), mailboxSourceSessionIDs([]db.AgentMailboxMessage{updated}))
	writeJSON(w, http.StatusOK, mailboxMessageToResponse(updated, sources[uuidToString(updated.SourceSessionID)]))
}

func validRetryClass(class string) bool {
	switch class {
	case "", "busy", "unavailable", "dependency":
		return true
	default:
		return false
	}
}

// mailboxAckState maps the frozen mailbox ack vocabulary onto the delivery
// core's outcome states.
func mailboxAckState(status string) (delivery.OutcomeState, bool) {
	switch status {
	case "delivered":
		return delivery.OutcomeDelivered, true
	case "retry":
		return delivery.OutcomeRetry, true
	case "failed":
		return delivery.OutcomeRejected, true
	default:
		return "", false
	}
}

// commentMentionDeliveryPayload is the immutable snapshot stored with a
// comment handoff. It deliberately contains stable references only: the
// discussion body stays in the native comment and is never copied into a
// second mutable message.
type commentMentionDeliveryPayload struct {
	Schema    string `json:"schema"`
	IssueID   string `json:"issue_id"`
	CommentID string `json:"comment_id"`
}

// mailboxDeliveryResults is the per-agent outcome of persisting a comment
// handoff. A queued entry means the delivery row committed; any other reason
// means the target was not addressed (for example no active session binding)
// and the comment response must say so instead of claiming a handoff.
type mailboxDeliveryResults map[string]DispatchReasonCode

// queuedMailboxAgents returns the agents whose delivery actually committed.
func (r mailboxDeliveryResults) queuedMailboxAgents() map[string]struct{} {
	out := make(map[string]struct{}, len(r))
	for agentID, reason := range r {
		if reason == ReasonQueued {
			out[agentID] = struct{}{}
		}
	}
	return out
}

// enqueueMailboxDeliveries persists one delivery per explicit external mention
// through the delivery core. q may be a transaction handle: CreateComment and
// UpdateComment pass their write transaction so the comment content and every
// valid handoff commit or roll back together. A persistence failure fails the
// whole call; the caller must not treat any target as queued.
//
// The session a handoff addresses is resolved by the application from the
// explicit (agent, issue) binding and then frozen on the delivery. A missing
// binding is reported per target and never falls back to another session of
// the same agent.
func (h *Handler) enqueueMailboxDeliveries(ctx context.Context, q *db.Queries, issue db.Issue, commentID pgtype.UUID, recipients []commentMentionTarget, actorType, actorID string) (mailboxDeliveryResults, error) {
	out := mailboxDeliveryResults{}
	if len(recipients) == 0 {
		return out, nil
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	issueID := uuidToString(issue.ID)
	var sourceSessionID string
	if actorType == "agent" {
		if binding, err := sessionroute.Resolve(ctx, q, workspaceID, actorID, issueID); err == nil {
			sourceSessionID = binding.SessionID
		}
	}
	store := delivery.NewPostgres(q)
	payload, err := json.Marshal(commentMentionDeliveryPayload{
		Schema:    "comment_mention.v1",
		IssueID:   issueID,
		CommentID: uuidToString(commentID),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal comment delivery payload: %w", err)
	}
	for _, target := range recipients {
		if target.MailboxAgent == nil {
			continue
		}
		agent := *target.MailboxAgent
		agentID := uuidToString(agent.ID)
		binding, err := sessionroute.Resolve(ctx, q, workspaceID, agentID, issueID)
		if errors.Is(err, sessionroute.ErrNotBound) {
			// The mention names an identity with no explicit target on this
			// issue. Fail the target, keep the comment: the author can bind a
			// session and ask again.
			out[agentID] = ReasonTargetUnavailable
			continue
		}
		if err != nil {
			return out, fmt.Errorf("resolve session binding for agent %s: %w", agentID, err)
		}
		_, err = store.Enqueue(ctx, delivery.EnqueueRequest{
			Scope: delivery.Scope{WorkspaceID: workspaceID},
			Source: delivery.Source{
				Type:      "comment",
				Key:       uuidToString(commentID),
				ActorType: actorType,
				ActorID:   actorID,
				SessionID: sourceSessionID,
			},
			Target:          delivery.Target{SessionID: binding.SessionID},
			Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
			NotificationKey: "comment:" + uuidToString(commentID) + ":agent:" + agentID,
			Kind:            "comment_mention",
			Payload:         payload,
			Refs: delivery.Refs{
				IssueID:   issueID,
				CommentID: uuidToString(commentID),
			},
		})
		if err != nil {
			// Keep already-persisted recipients in the result for diagnostics.
			// Callers own a transaction, so a failure rolls every one of them
			// back and none is reported as queued.
			return out, fmt.Errorf("enqueue comment delivery for agent %s: %w", agentID, err)
		}
		out[agentID] = ReasonQueued
	}
	return out, nil
}

// Mailbox state has its own lifecycle; invalidation avoids applying an issue
// snapshot with an unrelated revision or waking an issue workflow.
func (h *Handler) publishMailboxChanged(workspaceID pgtype.UUID, issueID string) {
	h.publish(protocol.EventIssueMailboxChanged, uuidToString(workspaceID), "system", "", map[string]any{"issue_id": issueID})
}
