package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// sessionTaskLimit bounds each list of the session task panel. A panel read is
// a glance, not a paginated work queue; the response says when it was cut.
const sessionTaskLimit = 50

// SessionTaskItem is one issue in the "this session's tasks" panel. It carries
// only what a card shows; the issue id and browser URL are the stable handles.
type SessionTaskItem struct {
	IssueID        string `json:"issue_id"`
	Identifier     string `json:"identifier,omitempty"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	StatusName     string `json:"status_name,omitempty"`
	StatusCategory string `json:"status_category"`
	URL            string `json:"url,omitempty"`
	// Role is why the item is on the list: executor, leader, acceptor, return, request.
	Role              string `json:"role"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	AcceptedAt        string `json:"accepted_at,omitempty"`
	TaskStatus        string `json:"task_status,omitempty"`
	ExecutorName      string `json:"executor_name,omitempty"`
	ActivityAt        string `json:"activity_at,omitempty"`
	DeliveryAt        string `json:"delivery_at,omitempty"`
	ChildCount        int32  `json:"child_count"`
	ClosedChildCount  int32  `json:"closed_child_count"`
	PendingAcceptance bool   `json:"pending_acceptance,omitempty"`
	BlockedReason     string `json:"blocked_reason,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	RequestAt         string `json:"request_at,omitempty"`
}

// SessionTasksResponse groups the three obligations of the credential's agent:
// work it owns, deliveries waiting for its acceptance, and delegations it
// accepted a return address for whose result has not come back yet.
type SessionTasksResponse struct {
	AgentID     string            `json:"agent_id"`
	SessionID   string            `json:"session_id,omitempty"`
	WorkspaceID string            `json:"workspace_id"`
	Owned       []SessionTaskItem `json:"owned"`
	Accepting   []SessionTaskItem `json:"accepting"`
	Returning   []SessionTaskItem `json:"returning"`
	Delegated   []SessionTaskItem `json:"delegated"`
	Requests    []SessionTaskItem `json:"requests"`
	// Truncated is true when any list hit the per-read limit and the panel's
	// counts therefore describe only the returned rows.
	Truncated bool `json:"truncated"`
}

func (h *Handler) GetAgentSessionTasks(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) && !isExternalAgentActor(r) {
		writeError(w, http.StatusForbidden, "this read requires a session credential or an authorized human")
		return
	}
	agent, ok := h.loadAgentForSessionRead(w, r)
	if !ok {
		return
	}
	sessionID, ok := h.selectSessionForTaskRead(w, r, agent)
	if !ok {
		return
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), agent.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	projection := &sessionTaskProjection{
		ctx:       r.Context(),
		queries:   h.statusCatalog(),
		workspace: workspace,
		appURL:    h.cfg.AppURL,
		statuses:  map[string]sessionTaskStatus{},
	}

	owned, ownedCut, err := h.sessionOwnedItems(r, projection, agent, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load owned tasks")
		return
	}
	accepting, acceptingCut, err := h.sessionAcceptingItems(r, projection, agent, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load acceptance work")
		return
	}
	returning, returningCut, err := h.sessionReturnItems(r, projection, agent, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pending returns")
		return
	}
	delegated, delegatedCut, err := h.sessionDelegatedItems(r, projection, agent, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load delegated tasks")
		return
	}
	requests, requestsCut, err := h.sessionRequests(r, projection, agent, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load requests")
		return
	}

	response := SessionTasksResponse{
		AgentID:     uuidToString(agent.ID),
		SessionID:   uuidToString(sessionID),
		WorkspaceID: uuidToString(workspace.ID),
		Owned:       owned,
		Accepting:   accepting,
		Returning:   returning,
		Delegated:   delegated,
		Requests:    requests,
		Truncated:   ownedCut || acceptingCut || returningCut || delegatedCut || requestsCut,
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) selectSessionForTaskRead(w http.ResponseWriter, r *http.Request, agent db.Agent) (pgtype.UUID, bool) {
	raw := r.URL.Query().Get("session_id")
	if isExternalAgentActor(r) {
		if raw != "" {
			writeError(w, http.StatusForbidden, "session credentials cannot select another session")
			return pgtype.UUID{}, false
		}
		return h.requireExternalActorSession(w, r)
	}
	sessionID, ok := parseUUIDOrBadRequest(w, raw, "session_id")
	if !ok {
		return pgtype.UUID{}, false
	}
	session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{
		ID: sessionID, WorkspaceID: agent.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && session.AgentID != agent.ID) {
		writeError(w, http.StatusNotFound, "session not found for this agent")
		return pgtype.UUID{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session")
		return pgtype.UUID{}, false
	}
	return sessionID, true
}

type archivedPaseoDelivery struct {
	ID              string `json:"id"`
	IssueIdentifier string `json:"issue_identifier"`
	IssueTitle      string `json:"issue_title"`
	HostID          string `json:"host_id"`
	SessionID       string `json:"session_id"`
}

func (h *Handler) ListArchivedPaseoDeliveries(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin", "member")
	if !ok {
		return
	}
	var sessionID pgtype.UUID
	if raw := r.URL.Query().Get("session_id"); raw != "" {
		sessionID, ok = parseUUIDOrBadRequest(w, raw, "session_id")
		if !ok {
			return
		}
		session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{
			ID: sessionID, WorkspaceID: workspaceUUID,
		})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && session.OwnerUserID != member.UserID && !roleAllowed(member.Role, "owner", "admin")) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load session")
			return
		}
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), workspaceUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	rows, err := h.Queries.ListArchivedPaseoDeliveries(r.Context(), db.ListArchivedPaseoDeliveriesParams{
		WorkspaceID: workspaceUUID, SessionID: sessionID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load archived deliveries")
		return
	}
	deliveries := make([]archivedPaseoDelivery, 0, len(rows))
	for _, row := range rows {
		deliveries = append(deliveries, archivedPaseoDelivery{
			ID: uuidToString(row.ID), IssueIdentifier: fmt.Sprintf("%s-%d", issuePrefixForWorkspace(workspace), row.IssueNumber),
			IssueTitle: row.IssueTitle, HostID: row.HostID, SessionID: row.SessionID,
		})
	}
	writeJSON(w, http.StatusOK, deliveries)
}

func (h *Handler) sessionRequests(r *http.Request, p *sessionTaskProjection, agent db.Agent, sessionID pgtype.UUID) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionUnansweredRequests(r.Context(), db.ListSessionUnansweredRequestsParams{
		WorkspaceID: agent.WorkspaceID, AgentID: agent.ID, ExternalSessionID: sessionID, RowLimit: sessionTaskLimit,
	})
	if err != nil {
		return nil, false, err
	}
	items := make([]SessionTaskItem, 0, len(rows))
	for _, row := range rows {
		item := p.item(uuidToString(row.IssueID), row.Number, row.Title, row.Status)
		item.Role = "request"
		item.RequestID = uuidToString(row.CommentID)
		item.RequestAt = timestampToString(row.CreatedAt)
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

func (h *Handler) sessionDelegatedItems(r *http.Request, p *sessionTaskProjection, agent db.Agent, sessionID pgtype.UUID) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionDelegatedIssues(r.Context(), db.ListSessionDelegatedIssuesParams{
		WorkspaceID: agent.WorkspaceID, AgentID: agent.ID, ExternalSessionID: sessionID, RowLimit: sessionTaskLimit,
	})
	if err != nil {
		return nil, false, err
	}
	items := make([]SessionTaskItem, 0, len(rows))
	for _, row := range rows {
		item := p.item(uuidToString(row.IssueID), row.Number, row.Title, row.IssueStatus)
		item.Role = "return"
		item.UpdatedAt = timestampToString(row.IssueUpdatedAt)
		item.ExecutorName = row.ExecutorName
		item.ActivityAt = timestampToString(row.ActivityAt)
		item.TaskStatus = row.TaskStatus
		if row.IssueStatus == "blocked" {
			item.BlockedReason = row.BlockedReason
		}
		var state issuecontract.State
		if len(row.ContractData) > 0 && json.Unmarshal(row.ContractData, &state) == nil {
			if pending := state.Pending(); pending != nil {
				item.PendingAcceptance = true
				item.ActivityAt = pending.CreatedAt.Format(time.RFC3339Nano)
			}
		}
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

func (h *Handler) statusCatalog() issuestatus.Querier {
	if h.IssueStatusCatalog != nil {
		return h.IssueStatusCatalog
	}
	return h.Queries
}

type sessionTaskStatus struct {
	category string
	name     string
}

// sessionTaskProjection derives the shared presentation fields once per read:
// identifier and browser URL from the workspace, status labels from the
// workspace catalog (one read per distinct status, not per issue).
type sessionTaskProjection struct {
	ctx       context.Context
	queries   issuestatus.Querier
	workspace db.Workspace
	appURL    string
	statuses  map[string]sessionTaskStatus
}

func (p *sessionTaskProjection) identifier(number int32) string {
	prefix := p.workspace.IssuePrefix
	if prefix == "" {
		return ""
	}
	return fmt.Sprintf("%s-%d", prefix, number)
}

func (p *sessionTaskProjection) status(status string) sessionTaskStatus {
	if cached, ok := p.statuses[status]; ok {
		return cached
	}
	category, name := issuestatus.CategoryAndName(p.ctx, p.queries, p.workspace.ID, status)
	meta := sessionTaskStatus{category: category, name: name}
	p.statuses[status] = meta
	return meta
}

func (p *sessionTaskProjection) item(issueID string, number int32, title, status string) SessionTaskItem {
	meta := p.status(status)
	identifier := p.identifier(number)
	return SessionTaskItem{
		IssueID:        issueID,
		Identifier:     identifier,
		Title:          title,
		Status:         status,
		StatusName:     meta.name,
		StatusCategory: meta.category,
		URL:            service.IssueBrowserURL(p.appURL, p.workspace.Slug, identifier),
	}
}

func (h *Handler) sessionOwnedItems(r *http.Request, p *sessionTaskProjection, agent db.Agent, sessionID pgtype.UUID) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionOwnedIssues(r.Context(), db.ListSessionOwnedIssuesParams{
		WorkspaceID:       agent.WorkspaceID,
		AgentID:           agent.ID,
		ExternalSessionID: sessionID,
		RowLimit:          sessionTaskLimit,
	})
	if err != nil {
		return nil, false, err
	}
	items := make([]SessionTaskItem, 0, len(rows))
	for _, row := range rows {
		item := p.item(uuidToString(row.ID), row.Number, row.Title, row.Status)
		item.Role = "leader"
		if row.IsExecutor {
			item.Role = "executor"
		}
		item.UpdatedAt = timestampToString(row.UpdatedAt)
		item.ChildCount = row.ChildCount
		item.ClosedChildCount = row.ClosedChildCount
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

func (h *Handler) sessionAcceptingItems(r *http.Request, p *sessionTaskProjection, agent db.Agent, sessionID pgtype.UUID) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionAcceptingContractIssues(r.Context(), db.ListSessionAcceptingContractIssuesParams{
		WorkspaceID:       agent.WorkspaceID,
		AgentID:           uuidToString(agent.ID),
		ExternalSessionID: uuidToString(sessionID),
		RowLimit:          sessionTaskLimit,
	})
	if err != nil {
		return nil, false, err
	}
	items := make([]SessionTaskItem, 0, len(rows))
	for _, row := range rows {
		var state issuecontract.State
		if err := json.Unmarshal(row.Data, &state); err != nil {
			continue
		}
		current := state.Current()
		if current == nil || current.Acceptor.ID != uuidToString(agent.ID) || current.Acceptor.SessionID != uuidToString(sessionID) || state.Pending() == nil {
			continue
		}
		item := p.item(uuidToString(row.ID), row.Number, row.Title, row.Status)
		item.Role = "acceptor"
		item.UpdatedAt = timestampToString(row.UpdatedAt)
		item.DeliveryAt = state.Pending().CreatedAt.Format(time.RFC3339Nano)
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

// sessionReturnItems lists the accepted return addresses this session
// dispatched and whose result has not come back. The return row is keyed by
// (issue, dispatcher), so one issue contributes at most one item.
func (h *Handler) sessionReturnItems(r *http.Request, p *sessionTaskProjection, agent db.Agent, sessionID pgtype.UUID) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionPendingDelegationReturns(r.Context(), db.ListSessionPendingDelegationReturnsParams{
		WorkspaceID:       agent.WorkspaceID,
		AgentID:           agent.ID,
		ExternalSessionID: sessionID,
		RowLimit:          sessionTaskLimit,
	})
	if err != nil {
		return nil, false, err
	}
	items := make([]SessionTaskItem, 0, len(rows))
	for _, row := range rows {
		item := p.item(uuidToString(row.IssueID), row.Number, row.Title, row.IssueStatus)
		item.Role = "return"
		item.UpdatedAt = timestampToString(row.IssueUpdatedAt)
		item.AcceptedAt = timestampToString(row.AcceptedAt)
		item.TaskStatus = row.TaskStatus
		item.ExecutorName = row.ExecutorName
		item.ActivityAt = timestampToString(row.ActivityAt)
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

// GetAgentSessionPullRequests returns the PRs linked to this session's work.
func (h *Handler) GetAgentSessionPullRequests(w http.ResponseWriter, r *http.Request) {
	if !isExternalAgentActor(r) {
		writeError(w, http.StatusForbidden, "this read is only available to a session credential")
		return
	}
	agent, ok := h.loadAgentForSessionRead(w, r)
	if !ok {
		return
	}
	sessionID, ok := h.requireExternalActorSession(w, r)
	if !ok {
		return
	}
	ws, err := h.Queries.GetWorkspace(r.Context(), agent.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	rows, err := h.DB.Query(r.Context(), `SELECT DISTINCT i.id, i.number FROM issue i
		LEFT JOIN issue root ON root.id = COALESCE(i.leadership_root_issue_id, i.id) AND root.workspace_id = i.workspace_id
		WHERE i.workspace_id = $1 AND (
		  (i.assignee_type = 'agent' AND i.assignee_id = $2 AND EXISTS
		    (SELECT 1 FROM external_agent_session_binding b WHERE b.workspace_id = i.workspace_id AND b.issue_id = i.id AND b.agent_id = $2 AND b.session_id = $3 AND b.revoked_at IS NULL))
		  OR (root.leader_agent_id = $2 AND root.leader_external_session_id = $3)
		  OR i.creator_session_id = $3
		  OR EXISTS (SELECT 1 FROM issue_delegation_return d WHERE d.workspace_id = i.workspace_id AND d.issue_id = i.id AND d.dispatcher_agent_id = $2 AND d.external_session_id = $3 AND d.is_current)
		) ORDER BY i.id`, agent.WorkspaceID, agent.ID, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session tasks")
		return
	}
	var roots []db.Issue
	for rows.Next() {
		var issue db.Issue
		if err = rows.Scan(&issue.ID, &issue.Number); err != nil {
			break
		}
		roots = append(roots, issue)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session tasks")
		return
	}
	out := make([]descendantPullRequest, 0)
	seen := make(map[string]bool)
	var direct []GitHubPullRequestResponse
	byIssue, err := h.batchIssuePullRequests(r.Context(), ws, roots, false)
	for _, issue := range roots {
		for _, pr := range byIssue[issue.ID] {
			if seen[pr.ID] {
				continue
			}
			seen[pr.ID] = true
			direct = append(direct, pr)
			identifier := fmt.Sprintf("%s-%d", issuePrefixForWorkspace(ws), issue.Number)
			out = append(out, descendantPullRequest{GitHubPullRequestResponse: pr, SourceIssueID: uuidToString(issue.ID), SourceIdentifier: identifier, SourceURL: service.IssueBrowserURL(h.cfg.AppURL, ws.Slug, identifier)})
		}
	}

	if err == nil {
		var descendants []descendantPullRequest
		descendants, err = h.descendantPullRequests(r.Context(), ws, roots, direct, false)
		out = append(out, descendants...)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session pull requests")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pull_requests": out})
}
