package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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
	Role       string `json:"role"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	AcceptedAt string `json:"accepted_at,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
}

// SessionTasksResponse groups the three obligations of the credential's agent:
// work it owns, deliveries waiting for its acceptance, and accepted
// delegations whose result it still owes.
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
	if _, ok := h.requireExternalActorSession(w, r); !ok {
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
	returning, returningCut, err := h.sessionReturnItems(r, projection, agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pending returns")
		return
	}

	response := SessionTasksResponse{
		AgentID:     uuidToString(agent.ID),
		WorkspaceID: uuidToString(workspace.ID),
		Owned:       owned,
		Accepting:   accepting,
		Returning:   returning,
		Truncated:   ownedCut || acceptingCut || returningCut,
	}
	if sid, ok := externalActorSessionID(r); ok {
		response.SessionID = uuidToString(sid)
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
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

func (h *Handler) sessionReturnItems(r *http.Request, p *sessionTaskProjection, agent db.Agent) ([]SessionTaskItem, bool, error) {
	rows, err := h.Queries.ListSessionReturnObligations(r.Context(), db.ListSessionReturnObligationsParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     agent.ID,
		RowLimit:    sessionTaskLimit,
	})
	if err != nil {
		return nil, false, err
	}
	items := make([]SessionTaskItem, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		issueID := uuidToString(row.IssueID)
		if seen[issueID] {
			continue
		}
		seen[issueID] = true
		item := p.item(issueID, row.Number, row.Title, row.IssueStatus)
		item.Role = "return"
		item.UpdatedAt = timestampToString(row.IssueUpdatedAt)
		item.AcceptedAt = timestampToString(row.AcceptedAt)
		item.TaskStatus = row.TaskStatus
		items = append(items, item)
	}
	return items, len(rows) == sessionTaskLimit, nil
}

