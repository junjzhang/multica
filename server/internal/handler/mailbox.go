package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
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

	mailboxLeaseDuration = 2 * time.Minute
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
	sources := h.externalSessionsByID(r.Context(), mailboxSourceSessionIDs([]db.AgentMailboxMessage{updated}))
	writeJSON(w, http.StatusOK, mailboxMessageToResponse(updated, sources[uuidToString(updated.SourceSessionID)]))
}

// daemonMailboxContext resolves the verified daemon identity for claim/ack.
// mdt_ requests carry workspace, daemon and owner in the auth context. PAT/JWT
// daemons carry only the human, so the workspace comes from the request
// selector and messages are narrowed to sessions that human registered.
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
	_, wsUUID, daemonID, ownerID, ok := h.daemonMailboxContext(w, r)
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

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start mailbox claim")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	ids, err := qtx.LockClaimableMailboxMessageIDs(r.Context(), db.LockClaimableMailboxMessageIDsParams{
		WorkspaceID: wsUUID,
		OwnerUserID: parseUUID(ownerID),
		DaemonID:    daemonID,
		RowLimit:    int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim mailbox")
		return
	}
	items := make([]daemonMailboxClaimItem, 0, len(ids))
	leaseExpires := time.Now().Add(mailboxLeaseDuration)
	for _, id := range ids {
		token, err := generateMailboxDeliveryToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to mint delivery token")
			return
		}
		leased, err := qtx.LeaseMailboxMessage(r.Context(), db.LeaseMailboxMessageParams{
			ID:             id,
			LeaseTokenHash: pgtype.Text{String: auth.HashToken(token), Valid: true},
			LeaseOwner:     pgtype.Text{String: leaseOwnerValue(daemonID, ownerID), Valid: true},
			LeaseExpiresAt: pgtype.Timestamptz{Time: leaseExpires, Valid: true},
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to lease mailbox message")
			return
		}
		session, err := qtx.GetExternalAgentSessionByAgentID(r.Context(), leased.RecipientAgentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load external session")
			return
		}
		content := h.renderMailboxNotification(r.Context(), leased)
		var source *ExternalSessionResponse
		if leased.SourceSessionID.Valid {
			if row, err := qtx.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{
				ID:          leased.SourceSessionID,
				WorkspaceID: wsUUID,
			}); err == nil {
				source = externalSessionToResponse(row)
			}
		}
		items = append(items, daemonMailboxClaimItem{
			Message:         mailboxMessageToResponse(leased, source),
			ExternalSession: *externalSessionToResponse(session),
			DeliveryToken:   token,
			LeaseExpiresAt:  timestampToString(pgtype.Timestamptz{Time: leaseExpires, Valid: true}),
			Content:         content,
		})
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit mailbox claim")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": items})
}

func leaseOwnerValue(daemonID, ownerID string) string {
	if daemonID != "" {
		return daemonID
	}
	return "owner:" + ownerID
}

func generateMailboxDeliveryToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// renderMailboxNotification is the text the host injects into the original
// session. It points at the issue and comment and never carries credentials.
func (h *Handler) renderMailboxNotification(ctx context.Context, m db.AgentMailboxMessage) string {
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
		ID:          m.IssueID,
		WorkspaceID: m.WorkspaceID,
	})
	if err != nil {
		return fmt.Sprintf("Mailbox request %s is no longer available (issue deleted).", uuidToString(m.ID))
	}
	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	identifier := prefix + "-" + strconv.Itoa(int(issue.Number))
	comment, err := h.Queries.GetComment(ctx, m.CommentID)
	body := ""
	if err == nil {
		body = comment.Content
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Mailbox request on %s: %s\n", identifier, issue.Title)
	fmt.Fprintf(&b, "Issue ID: %s\nComment ID: %s\nRequest:\n%s\n\n", uuidToString(issue.ID), uuidToString(m.CommentID), body)
	b.WriteString("Reply by commenting on the issue. Mention the requester or coordinator explicitly if they need to act; a plain reply does not start anyone.")
	return b.String()
}

type ackDaemonMailboxRequest struct {
	DeliveryToken string `json:"delivery_token"`
	Status        string `json:"status"`
	HostReceipt   string `json:"host_receipt"`
	Error         string `json:"error"`
}

var mailboxSecretPattern = regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._-]+|\b(?:mea|mat|mdt|mul|mcn)_[a-z0-9]+)`)

// sanitizeMailboxDiagnostic bounds stored diagnostics and strips anything that
// looks like a bearer credential. Delivery errors are persisted and projected
// to humans, so a host must not be able to leak its secrets through them.
func sanitizeMailboxDiagnostic(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = mailboxSecretPattern.ReplaceAllString(s, "[redacted]")
	if len(s) > 2000 {
		s = s[:2000]
	}
	return s
}

// AckDaemonMailbox records the daemon's delivery result. The lease token fences
// the write: a stale or foreign ack matches no row and cannot overwrite a later
// lease.
func (h *Handler) AckDaemonMailbox(w http.ResponseWriter, r *http.Request) {
	_, wsUUID, daemonID, _, ok := h.daemonMailboxContext(w, r)
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
	switch req.Status {
	case "delivered", "retry", "failed":
	default:
		writeError(w, http.StatusBadRequest, "status must be 'delivered', 'retry' or 'failed'")
		return
	}
	message, err := h.Queries.GetAgentMailboxMessage(r.Context(), db.GetAgentMailboxMessageParams{
		ID:          messageID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "mailbox message not found")
		return
	}
	var nextAttempt pgtype.Timestamptz
	if req.Status == "retry" {
		nextAttempt = pgtype.Timestamptz{Time: retryBackoff(message.Attempts), Valid: true}
	}
	// A user-credential daemon (PAT/JWT) has no stable daemon id; the delivery
	// token still fences the lease. An mdt_ daemon must also match the lease
	// owner recorded at claim time.
	updated, err := h.Queries.AckAgentMailboxDelivery(r.Context(), db.AckAgentMailboxDeliveryParams{
		ID:             messageID,
		WorkspaceID:    wsUUID,
		Status:         req.Status,
		HostReceipt:    pgtype.Text{String: sanitizeMailboxDiagnostic(req.HostReceipt), Valid: req.HostReceipt != ""},
		LastError:      pgtype.Text{String: sanitizeMailboxDiagnostic(req.Error), Valid: req.Error != ""},
		NextAttemptAt:  nextAttempt,
		LeaseTokenHash: pgtype.Text{String: auth.HashToken(req.DeliveryToken), Valid: true},
		DaemonID:       daemonID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "delivery lease is stale or does not belong to this daemon")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to acknowledge delivery")
		return
	}
	sources := h.externalSessionsByID(r.Context(), mailboxSourceSessionIDs([]db.AgentMailboxMessage{updated}))
	writeJSON(w, http.StatusOK, mailboxMessageToResponse(updated, sources[uuidToString(updated.SourceSessionID)]))
}

// retryBackoff grows with attempts and is capped so a permanently unreachable
// host keeps retrying at a bounded interval instead of hot-looping.
func retryBackoff(attempts int32) time.Time {
	seconds := int64(attempts) * int64(attempts) * 15
	if seconds < 15 {
		seconds = 15
	}
	if seconds > 15*60 {
		seconds = 15 * 60
	}
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

// createMailboxDeliveries persists one request per explicit external mention.
// Re-delivery of the same comment is idempotent through the unique key.
func (h *Handler) createMailboxDeliveries(ctx context.Context, issue db.Issue, comment db.Comment, recipients []commentMentionTarget, actorType, actorID string) map[string]db.AgentMailboxMessage {
	out := map[string]db.AgentMailboxMessage{}
	var sourceSessionID pgtype.UUID
	if actorType == "agent" {
		if session, err := h.Queries.GetExternalAgentSessionByAgentID(ctx, parseUUID(actorID)); err == nil {
			sourceSessionID = session.ID
		}
	}
	for _, target := range recipients {
		if target.MailboxAgent == nil {
			continue
		}
		agent := *target.MailboxAgent
		created, err := h.Queries.CreateAgentMailboxMessage(ctx, db.CreateAgentMailboxMessageParams{
			ID:               dbid.NewV7(),
			WorkspaceID:      issue.WorkspaceID,
			IssueID:          issue.ID,
			CommentID:        comment.ID,
			RecipientAgentID: agent.ID,
			SourceSessionID:  sourceSessionID,
			SenderType:       actorType,
			SenderID:         parseUUID(actorID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := h.Queries.GetAgentMailboxMessageByComment(ctx, db.GetAgentMailboxMessageByCommentParams{
				RecipientAgentID: agent.ID,
				CommentID:        comment.ID,
			})
			if getErr == nil {
				out[uuidToString(agent.ID)] = existing
			}
			continue
		}
		if err != nil {
			continue
		}
		out[uuidToString(agent.ID)] = created
	}
	return out
}
