package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/squadbriefing"
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
	HandledByTaskID  string                   `json:"handled_by_task_id,omitempty"`
	Payload          json.RawMessage          `json:"payload,omitempty"`
	IssueID          string                   `json:"issue_id"`
	CommentID        string                   `json:"comment_id"`
	RecipientAgentID string                   `json:"recipient_agent_id"`
	SourceSession    *ExternalSessionResponse `json:"source_session"`
	TargetSession    *ExternalSessionResponse `json:"target_session"`
	SourceType       string                   `json:"source_type"`
	Kind             string                   `json:"kind"`
	DeliveryStatus   string                   `json:"delivery_status"`
	ProcessingStatus string                   `json:"processing_status"`
	CreatedAt        string                   `json:"created_at"`
	DeliveredAt      *string                  `json:"delivered_at"`
	LastError        *string                  `json:"last_error"`
}

func mailboxMessageToResponse(m db.AgentMailboxMessage, source, target *ExternalSessionResponse) MailboxMessageResponse {
	return MailboxMessageResponse{
		ID:               uuidToString(m.ID),
		Payload:          m.Payload,
		HandledByTaskID:  uuidToString(m.HandledByTaskID),
		IssueID:          uuidToString(m.IssueID),
		CommentID:        uuidToString(m.CommentID),
		RecipientAgentID: uuidToString(m.RecipientAgentID),
		SourceSession:    source,
		TargetSession:    target,
		SourceType:       m.SourceType,
		Kind:             m.Kind,
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

	daemonDeliveryClaimDefault = 10
	daemonDeliveryClaimMax     = 50
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

// loadMailboxAgentForActor resolves the authenticated recipient or human manager.
// Managed credentials additionally require a live pinned execution.
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
	if err != nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return db.Agent{}, false
	}
	if r.Header.Get("X-Actor-Source") == "task_token" {
		if r.Header.Get("X-Agent-ID") != uuidToString(agent.ID) || agent.ExecutionKind != "managed" {
			writeError(w, http.StatusForbidden, "task credentials may only access their own mailbox")
			return db.Agent{}, false
		}
		if _, err := delegation.ResolveManaged(r.Context(), h.Queries, workspaceID, uuidToString(agent.ID), r.Header.Get("X-Task-ID")); err != nil {
			writeError(w, http.StatusForbidden, "mailbox requires a live pinned task")
			return db.Agent{}, false
		}
		return agent, true
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
	var actorSession pgtype.UUID
	if isExternalAgentActor(r) {
		id, ok := h.requireExternalActorSession(w, r)
		if !ok {
			return
		}
		actorSession = id
	}
	sourceType := r.URL.Query().Get("kind")
	if sourceType == "" {
		sourceType = "comment"
		if agent.ExecutionKind == "managed" {
			sourceType = delegation.ResultTaskType
		}
	}
	if sourceType != "comment" && sourceType != delegation.ResultTaskType && (sourceType != "background_wait" || agent.ExecutionKind != "external") {
		writeError(w, http.StatusBadRequest, "kind must be comment, delegation_result, or background_wait (external only)")
		return
	}
	var caller db.AgentTaskQueue
	managedActor := r.Header.Get("X-Actor-Source") == "task_token"
	if managedActor {
		var err error
		caller, err = h.Queries.GetAgentTask(r.Context(), parseUUID(r.Header.Get("X-Task-ID")))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to verify mailbox consumer")
			return
		}
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
		SourceType:       sourceType,
		WorkspaceID:      agent.WorkspaceID,
		TargetSessionID:  actorSession,
		AfterCreatedAt:   afterCreated,
		AfterID:          afterID,
		PageLimit:        int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mailbox")
		return
	}
	sessions := h.externalSessionsByID(r.Context(), mailboxDeliverySessionIDs(rows))
	messages := make([]MailboxMessageResponse, 0, len(rows))
	for _, row := range rows {
		if managedActor {
			visible, err := service.ManagedMailboxResultVisible(r.Context(), h.Queries, row, caller)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to verify mailbox scope")
				return
			}
			if !visible {
				continue
			}
		}
		messages = append(messages, mailboxMessageToResponse(
			row,
			sessions[uuidToString(row.SourceSessionID)],
			sessions[uuidToString(row.TargetSessionID)],
		))
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

func mailboxDeliverySessionIDs(rows []db.AgentMailboxMessage) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(rows)*2)
	seen := make(map[string]struct{}, len(rows)*2)
	add := func(id pgtype.UUID) {
		if !id.Valid {
			return
		}
		key := uuidToString(id)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	for _, row := range rows {
		add(row.SourceSessionID)
		add(row.TargetSessionID)
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
	if agent.ExecutionKind == "managed" && r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "managed result consumption requires the receiving task credential")
		return
	}
	var actorSession pgtype.UUID
	if isExternalAgentActor(r) {
		id, ok := h.requireExternalActorSession(w, r)
		if !ok {
			return
		}
		actorSession = id
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
	var callerID pgtype.UUID
	if r.Header.Get("X-Actor-Source") == "task_token" {
		callerID = parseUUID(r.Header.Get("X-Task-ID"))
	}
	updated, err := h.TaskService.AckMailbox(r.Context(), db.AckAgentMailboxProcessingParams{
		ID:               messageID,
		WorkspaceID:      agent.WorkspaceID,
		RecipientAgentID: agent.ID,
		TargetSessionID:  actorSession,
		State:            req.State,
	}, callerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "mailbox message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to acknowledge mailbox message")
		return
	}
	h.publishMailboxChanged(updated.WorkspaceID, uuidToString(updated.IssueID))
	sessions := h.externalSessionsByID(r.Context(), mailboxDeliverySessionIDs([]db.AgentMailboxMessage{updated}))
	writeJSON(w, http.StatusOK, mailboxMessageToResponse(
		updated,
		sessions[uuidToString(updated.SourceSessionID)],
		sessions[uuidToString(updated.TargetSessionID)],
	))
}

// daemonMailboxContext resolves the verified daemon identity for delivery
// claim/outcome.
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

func validRetryClass(class string) bool {
	switch class {
	case "", "busy", "unavailable", "dependency":
		return true
	default:
		return false
	}
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
	byAgent := make(map[string]commentMentionTarget, len(recipients))
	var order []string
	for _, target := range recipients {
		if target.MailboxAgent == nil {
			continue
		}
		id := uuidToString(target.MailboxAgent.ID)
		existing, seen := byAgent[id]
		if !seen {
			order = append(order, id)
		}
		// A squad role takes precedence over a direct mention, just as it
		// does for managed triggers. Multiple squads retain the first role.
		if !seen || (existing.TargetType != "squad" && target.TargetType == "squad") {
			byAgent[id] = target
		}
	}
	for _, id := range order {
		target := byAgent[id]
		if target.MailboxAgent == nil {
			continue
		}
		agent := *target.MailboxAgent
		agentID := uuidToString(agent.ID)
		notificationKey := "comment:" + uuidToString(commentID) + ":agent:" + agentID
		existing, loadErr := q.GetAgentMailboxMessageByComment(ctx, db.GetAgentMailboxMessageByCommentParams{RecipientAgentID: agent.ID, CommentID: commentID})
		if loadErr == nil && len(existing.Payload) > 0 && existing.TargetSessionID.Valid {
			_, replayErr := store.Enqueue(ctx, delivery.EnqueueRequest{
				Scope:           delivery.Scope{WorkspaceID: workspaceID},
				Source:          delivery.Source{Type: "comment", Key: uuidToString(commentID), ActorType: actorType, ActorID: actorID, SessionID: sourceSessionID},
				Target:          delivery.Target{SessionID: uuidToString(existing.TargetSessionID)},
				Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
				NotificationKey: notificationKey, Kind: "comment_mention", Payload: existing.Payload,
				Refs: delivery.Refs{IssueID: issueID, CommentID: uuidToString(commentID)},
			})
			if replayErr != nil {
				return out, replayErr
			}
			out[agentID] = ReasonQueued
			continue
		}
		if loadErr != nil && !errors.Is(loadErr, pgx.ErrNoRows) {
			return out, loadErr
		}
		payload := protocol.ExternalIssueRequest{Schema: "comment_mention.v1", IssueID: issueID, CommentID: uuidToString(commentID), Mode: target.Mode}
		if display := service.ResolveDeliveryDisplay(ctx, q, issue, h.cfg.AppURL); display.Identifier != "" || display.Title != "" {
			payload.IssueIdentifier, payload.IssueTitle, payload.IssueURL = display.Identifier, display.Title, display.URL
		}
		if target.TargetType == "squad" {
			squad, err := q.LockSquadForAutopilotAssignment(ctx, db.LockSquadForAutopilotAssignmentParams{ID: parseUUID(target.TargetID), WorkspaceID: issue.WorkspaceID})
			leader, leaderErr := issueleader.ResolveIssueLeader(ctx, q, issue, squad)
			if err != nil || leaderErr != nil || leader.Agent.ID != agent.ID {
				out[agentID] = ReasonTargetUnavailable
				continue
			}
			payload.SquadID = uuidToString(squad.ID)
			payload.Briefing = squadbriefing.BuildForIssue(ctx, q, issue, squad, issue.AssigneeType.String == "squad" && issue.AssigneeID == squad.ID, squadbriefing.Options{})
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return out, err
		}
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
			NotificationKey: notificationKey,
			Kind:            "comment_mention",
			Payload:         body,
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
