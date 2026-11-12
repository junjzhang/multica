package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AgentSessionBindingResponse is the frozen wire shape for one explicit
// (agent, issue) -> session target. Timestamps are presentation only; the
// binding id is the stable handle.
type AgentSessionBindingResponse struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	IssueID   string `json:"issue_id"`
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
}

type bindAgentSessionRequest struct {
	IssueID   string `json:"issue_id"`
	SessionID string `json:"session_id"`
}

func agentSessionBindingToResponse(binding sessionroute.Binding) AgentSessionBindingResponse {
	return AgentSessionBindingResponse{
		ID:        binding.ID,
		AgentID:   binding.AgentID,
		IssueID:   binding.IssueID,
		SessionID: binding.SessionID,
		CreatedAt: binding.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// BindAgentSession records which session of an agent serves one issue, and
// revokes the previous active binding of the same exact key. Newly addressed
// notifications use the replacement. Accepted delegation returns and earlier
// delivery envelopes retain their original targets.
func (h *Handler) BindAgentSession(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	agentID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "agent id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if agent.ExecutionKind != "external" {
		writeError(w, http.StatusBadRequest, "only external agents have host sessions to bind")
		return
	}
	if !h.canManageAgent(w, r, agent) {
		return
	}

	var req bindAgentSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.IssueID = strings.TrimSpace(req.IssueID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.IssueID == "" || req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "issue_id and session_id are required")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to bind session")
		return
	}
	defer tx.Rollback(r.Context())

	issueUUID, ok := parseUUIDOrBadRequest(w, req.IssueID, "issue_id")
	if !ok {
		return
	}
	q := h.Queries.WithTx(tx)
	issue, err := q.LockIssueForDescriptionUpdate(r.Context(), db.LockIssueForDescriptionUpdateParams{ID: issueUUID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, 404, "issue not found")
		return
	}
	root, err := q.GetIssueLeadershipRoot(r.Context(), db.GetIssueLeadershipRootParams{ID: issue.ID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, 500, "load task leadership failed")
		return
	}
	if issue.AssigneeType.String == "agent" && issue.AssigneeID == agentID || root.LeaderAgentID == agentID {
		writeError(w, 409, "executor bindings may only change through assignment or explicit leadership handoff")
		return
	}
	binding, err := sessionroute.Bind(r.Context(), h.Queries.WithTx(tx), sessionroute.BindRequest{
		WorkspaceID: workspaceID,
		AgentID:     uuidToString(agentID),
		IssueID:     req.IssueID,
		SessionID:   req.SessionID,
		CreatedBy:   userID,
	})
	if err != nil {
		if errors.Is(err, sessionroute.ErrInvalidBinding) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A concurrent bind of the same key revoked and re-created the row
		// between our revoke and insert. The unique active-binding index is
		// the arbiter; report the lost race, not a server fault.
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "the session binding changed concurrently; retry")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to bind session")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to bind session")
		return
	}
	writeJSON(w, http.StatusCreated, agentSessionBindingToResponse(binding))
}

// AgentSessionSummaryResponse is one registered host session of an agent plus
// whether new notifications may use it right now. It is a read projection: the
// session row itself is immutable identity, while deliverability is computed
// from the agent and owner state on every read.
type AgentSessionSummaryResponse struct {
	ExternalSessionResponse
	Deliverable       bool   `json:"deliverable"`
	UnavailableReason string `json:"unavailable_reason"`
}

// AgentSessionBindingViewResponse names the exact active target for one
// (agent, issue) pair. Existing notifications keep their frozen target, so this
// is the destination for FUTURE notifications only.
type AgentSessionBindingViewResponse struct {
	ID                string                   `json:"id"`
	AgentID           string                   `json:"agent_id"`
	IssueID           string                   `json:"issue_id"`
	Session           *ExternalSessionResponse `json:"session"`
	Deliverable       bool                     `json:"deliverable"`
	UnavailableReason string                   `json:"unavailable_reason"`
	CreatedAt         string                   `json:"created_at"`
}

// loadAgentForSessionRead admits the agent's own external credential or a
// human who can manage it. Any other caller and any other workspace is denied
// before a session row is read.
func (h *Handler) loadAgentForSessionRead(w http.ResponseWriter, r *http.Request) (db.Agent, bool) {
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
			writeError(w, http.StatusForbidden, "session credentials may only read their own agent")
			return db.Agent{}, false
		}
		return agent, true
	}
	if !h.canManageAgent(w, r, agent) {
		return db.Agent{}, false
	}
	return agent, true
}

func sessionAvailability(agentArchived, ownerIsMember, sessionRevoked bool) (bool, string) {
	if sessionRevoked {
		return false, "session_revoked"
	}
	if agentArchived {
		return false, "agent_archived"
	}
	if !ownerIsMember {
		return false, "owner_not_member"
	}
	return true, ""
}

// ListAgentSessions returns every registered session for one external agent.
// A missing binding is not an error and an archived agent still lists its
// sessions, marked undeliverable, so the task view can explain the state.
func (h *Handler) ListAgentSessions(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadAgentForSessionRead(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListExternalAgentSessionsForRead(r.Context(), db.ListExternalAgentSessionsForReadParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     agent.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list agent sessions")
		return
	}
	sessions := make([]AgentSessionSummaryResponse, 0, len(rows))
	for _, row := range rows {
		deliverable, reason := sessionAvailability(row.AgentArchived, row.OwnerIsMember, row.SessionRevoked)
		sessions = append(sessions, AgentSessionSummaryResponse{
			ExternalSessionResponse: ExternalSessionResponse{
				ID:          uuidToString(row.ID),
				Provider:    row.Provider,
				HostID:      row.HostID,
				SessionID:   row.SessionID,
				DisplayName: row.DisplayName,
				URL:         textToPtr(row.Url),
				DaemonID:    row.DaemonID,
			},
			Deliverable:       deliverable,
			UnavailableReason: reason,
		})
	}
	total := len(sessions)
	// Absent limit keeps the pre-pagination contract: every session. A caller
	// that asks for a page gets one bounded page plus the true total.
	limit, offset := total, 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed <= 0 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 100")
			return
		}
		limit = parsed
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = parsed
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions[offset:end], "total": total})
}

// GetAgentSessionBinding reads the exact active (workspace, agent, issue)
// target. A valid issue with no binding answers 200 with a null binding: that
// is an addressable state, not a server failure.
func (h *Handler) GetAgentSessionBinding(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadAgentForSessionRead(w, r)
	if !ok {
		return
	}
	issueID := strings.TrimSpace(r.URL.Query().Get("issue_id"))
	if issueID == "" {
		writeError(w, http.StatusBadRequest, "issue_id is required")
		return
	}
	issueUUID, ok := parseUUIDOrBadRequest(w, issueID, "issue_id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID:          issueUUID,
		WorkspaceID: agent.WorkspaceID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	row, err := h.Queries.GetActiveAgentSessionBindingView(r.Context(), db.GetActiveAgentSessionBindingViewParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     agent.ID,
		IssueID:     issueUUID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"binding": nil})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read session binding")
		return
	}
	binding := sessionBindingViewToResponse(sessionBindingView{
		BindingID:      row.BindingID,
		AgentID:        row.AgentID,
		IssueID:        row.IssueID,
		SessionID:      row.SessionID,
		CreatedAt:      row.CreatedAt,
		Provider:       row.Provider,
		HostID:         row.HostID,
		HostSessionID:  row.HostSessionID,
		DisplayName:    row.DisplayName,
		URL:            row.Url,
		DaemonID:       row.DaemonID,
		AgentArchived:  row.AgentArchived,
		OwnerIsMember:  row.OwnerIsMember,
		SessionRevoked: row.SessionRevoked,
	})
	writeJSON(w, http.StatusOK, map[string]any{"binding": binding})
}

// sessionBindingView flattens the two generated row shapes that join a
// binding to its frozen session, so the wire projection is written once.
type sessionBindingView struct {
	BindingID      pgtype.UUID
	AgentID        pgtype.UUID
	IssueID        pgtype.UUID
	SessionID      pgtype.UUID
	CreatedAt      pgtype.Timestamptz
	Provider       string
	HostID         string
	HostSessionID  string
	DisplayName    string
	URL            pgtype.Text
	DaemonID       string
	AgentArchived  bool
	OwnerIsMember  bool
	SessionRevoked bool
}

func sessionBindingViewToResponse(view sessionBindingView) AgentSessionBindingViewResponse {
	deliverable, reason := sessionAvailability(view.AgentArchived, view.OwnerIsMember, view.SessionRevoked)
	return AgentSessionBindingViewResponse{
		ID:      uuidToString(view.BindingID),
		AgentID: uuidToString(view.AgentID),
		IssueID: uuidToString(view.IssueID),
		Session: &ExternalSessionResponse{
			ID:          uuidToString(view.SessionID),
			Provider:    view.Provider,
			HostID:      view.HostID,
			SessionID:   view.HostSessionID,
			DisplayName: view.DisplayName,
			URL:         textToPtr(view.URL),
			DaemonID:    view.DaemonID,
		},
		Deliverable:       deliverable,
		UnavailableReason: reason,
		CreatedAt:         timestampToString(view.CreatedAt),
	}
}
