package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
	// Role is why the item is on the list: executor, leader, acceptor, return.
	Role             string `json:"role"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	AcceptedAt       string `json:"accepted_at,omitempty"`
	TaskStatus       string `json:"task_status,omitempty"`
	ExecutorName     string `json:"executor_name,omitempty"`
	ActivityAt       string `json:"activity_at,omitempty"`
	DeliveryAt       string `json:"delivery_at,omitempty"`
	ChildCount       int32  `json:"child_count"`
	ClosedChildCount int32  `json:"closed_child_count"`
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
	// Truncated is true when any list hit the per-read limit and the panel's
	// counts therefore describe only the returned rows.
	Truncated bool `json:"truncated"`
}

// GetAgentSessionTasks reads the task summary for one agent's session. It is a
// read projection only: no obligation changes, and the caller must present the
// agent's own session-bound external credential.
//
// Owned and accepting items are agent-scoped: an external agent registers one
// session per conversation, so its work is this session's work. A multi-session
// agent would need a session-scoped projection instead.
func (h *Handler) GetAgentSessionTasks(w http.ResponseWriter, r *http.Request) {
	// The projection is the session's own view: a human manager, a task token
	// or a sibling agent credential has no session to summarize for.
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

	owned, ownedCut, err := h.sessionOwnedItems(r, projection, agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load owned tasks")
		return
	}
	accepting, acceptingCut, err := h.sessionAcceptingItems(r, projection, agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load acceptance work")
		return
	}
	returning, returningCut, err := h.sessionReturnItems(r, projection, agent, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pending returns")
		return
	}

	response := SessionTasksResponse{
		AgentID:     uuidToString(agent.ID),
		SessionID:   uuidToString(sessionID),
		WorkspaceID: uuidToString(workspace.ID),
		Owned:       owned,
		Accepting:   accepting,
		Returning:   returning,
		Truncated:   ownedCut || acceptingCut || returningCut,
	}
	writeJSON(w, http.StatusOK, response)
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

func (h *Handler) sessionOwnedItems(r *http.Request, p *sessionTaskProjection, agent db.Agent) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionOwnedIssues(r.Context(), db.ListSessionOwnedIssuesParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     agent.ID,
		RowLimit:    sessionTaskLimit,
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

func (h *Handler) sessionAcceptingItems(r *http.Request, p *sessionTaskProjection, agent db.Agent) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionAcceptingContractIssues(r.Context(), db.ListSessionAcceptingContractIssuesParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     uuidToString(agent.ID),
		RowLimit:    sessionTaskLimit,
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
		if current == nil || current.Acceptor.ID != uuidToString(agent.ID) || state.Pending() == nil {
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
