package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
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
// revokes the previous active binding of the same exact key. It is the explicit
// takeover entry: earlier delivery rows keep the target they froze, while
// later notifications resolve the replacement.
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
