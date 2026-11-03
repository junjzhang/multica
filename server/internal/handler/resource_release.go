package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (h *Handler) resourceReleaseIdentity(w http.ResponseWriter, r *http.Request, req protocol.ResourceReleaseRequest) (pgtype.UUID, string, bool) {
	workspaceID := req.WorkspaceID
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if req.DaemonID != "" {
		if daemonID != "" && daemonID != req.DaemonID {
			writeError(w, 409, "resource daemon identity mismatch")
			return pgtype.UUID{}, "", false
		}
		daemonID = req.DaemonID
	}
	runtimeID := req.RuntimeID
	if req.SourceTaskID != "" {
		id, ok := parseUUIDOrBadRequest(w, req.SourceTaskID, "source_task_id")
		if !ok {
			return pgtype.UUID{}, "", false
		}
		task, err := h.Queries.GetAgentTask(r.Context(), id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 500, "load source task failed")
			return pgtype.UUID{}, "", false
		}
		if err == nil {
			if runtimeID != "" && runtimeID != uuidToString(task.RuntimeID) {
				writeError(w, 409, "source runtime mismatch")
				return pgtype.UUID{}, "", false
			}
			runtimeID = uuidToString(task.RuntimeID)
		}
	}
	var resourceAgent pgtype.UUID
	if req.Kind == "provider_store" {
		id, ok := parseUUIDOrBadRequest(w, req.AgentID, "agent_id")
		if !ok {
			return pgtype.UUID{}, "", false
		}
		resourceAgent = id
		actor, err := h.Queries.GetAgent(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 404, "resource agent not found")
			return pgtype.UUID{}, "", false
		}
		if err != nil {
			writeError(w, 500, "load resource agent failed")
			return pgtype.UUID{}, "", false
		}
		if workspaceID != "" && workspaceID != uuidToString(actor.WorkspaceID) {
			writeError(w, 404, "resource workspace not found")
			return pgtype.UUID{}, "", false
		}
		workspaceID = uuidToString(actor.WorkspaceID)
		if req.ScopeType != "agent" {
			if _, ok := parseUUIDOrBadRequest(w, req.ScopeID, "scope_id"); !ok {
				return pgtype.UUID{}, "", false
			}
		}
	}
	if runtimeID != "" {
		rt, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
		if !ok {
			return pgtype.UUID{}, "", false
		}
		if workspaceID != "" && workspaceID != uuidToString(rt.WorkspaceID) {
			writeError(w, 404, "workspace not found")
			return pgtype.UUID{}, "", false
		}
		if daemonID != "" && daemonID != rt.DaemonID.String {
			writeError(w, 409, "resource daemon identity mismatch")
			return pgtype.UUID{}, "", false
		}
		workspaceID, daemonID = uuidToString(rt.WorkspaceID), rt.DaemonID.String
		if req.Provider != "" && req.Provider != rt.Provider {
			writeError(w, 409, "resource provider mismatch")
			return pgtype.UUID{}, "", false
		}
	}
	ws, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return pgtype.UUID{}, "", false
	}
	if !h.requireDaemonWorkspaceAccess(w, r, workspaceID) {
		return pgtype.UUID{}, "", false
	}
	if daemonID == "" {
		writeError(w, 409, "resource daemon identity unavailable")
		return pgtype.UUID{}, "", false
	}
	owners, err := h.Queries.ListRuntimeOwnersByDaemon(r.Context(), db.ListRuntimeOwnersByDaemonParams{WorkspaceID: ws, DaemonID: pgtype.Text{String: daemonID, Valid: true}})
	if err != nil {
		writeError(w, 500, "load resource daemon ownership failed")
		return pgtype.UUID{}, "", false
	}
	if len(owners) == 0 {
		writeError(w, 409, "resource daemon ownership unavailable")
		return pgtype.UUID{}, "", false
	}
	for _, owner := range owners {
		if !daemonRuntimeBindingAllowed(r, pgtype.Text{String: daemonID, Valid: true}, owner.OwnerID) {
			writeError(w, 404, "resource daemon not found")
			return pgtype.UUID{}, "", false
		}
	}
	if resourceAgent.Valid {
		hosted, err := h.Queries.HasAgentResourceOnDaemon(r.Context(), db.HasAgentResourceOnDaemonParams{WorkspaceID: ws, DaemonID: pgtype.Text{String: daemonID, Valid: true}, Provider: req.Provider, AgentID: resourceAgent})
		if err != nil {
			writeError(w, 500, "load resource ownership failed")
			return pgtype.UUID{}, "", false
		}
		if !hosted {
			writeError(w, 404, "resource agent is not hosted by this daemon")
			return pgtype.UUID{}, "", false
		}
	}
	return ws, daemonID, true
}

func validResourceRelease(req protocol.ResourceReleaseRequest) bool {
	switch req.Kind {
	case "task":
		return req.SourceTaskID != ""
	case "workdir":
		return req.SourceTaskID != "" && strings.TrimSpace(req.ResourcePath) != ""
	case "provider_store":
		return req.Provider != "" && req.AgentID != "" && (req.ScopeType == "agent" || ((req.ScopeType == "issue" || req.ScopeType == "chat" || req.ScopeType == "autopilot") && req.ScopeID != ""))
	}
	return false
}

func (h *Handler) ReleaseManagedResource(w http.ResponseWriter, r *http.Request) {
	var req protocol.ResourceReleaseRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || !validResourceRelease(req) {
		writeError(w, 400, "invalid resource release request")
		return
	}
	workspace, daemonID, ok := h.resourceReleaseIdentity(w, r, req)
	if !ok {
		return
	}
	decision, err := h.TaskService.ResourceRelease(r.Context(), workspace, daemonID, req)
	if err != nil {
		writeError(w, 500, "resource dependencies unavailable")
		return
	}
	writeJSON(w, http.StatusOK, decision)
}

func (h *Handler) RecordResourcesReleased(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req protocol.ResourceReleaseReceipt
	if json.NewDecoder(r.Body).Decode(&req) != nil || !validResourceRelease(req.ResourceReleaseRequest) || strings.TrimSpace(req.DurableWorkDir) == "" {
		writeError(w, 400, "invalid resource release receipt")
		return
	}
	if req.SourceTaskID != uuidToString(task.ID) {
		link, err := h.Queries.GetDelegationResultObligationForTask(r.Context(), task.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 409, "resource receipt task mismatch")
			return
		}
		if err != nil {
			writeError(w, 500, "load resource receipt ownership failed")
			return
		}
		address, err := h.Queries.GetDelegationReturnByID(r.Context(), link.DelegationReturnID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 500, "load resource receipt return failed")
			return
		}
		if err != nil || address.ReturnKind != "managed_task" || uuidToString(address.SourceTaskID) != req.SourceTaskID || address.DispatcherAgentID != task.AgentID || address.RuntimeID != task.RuntimeID {
			writeError(w, 409, "resource receipt task mismatch")
			return
		}
	}
	if req.WorkDir == task.WorkDir.String && task.DurableWorkDir.Valid && task.DurableWorkDir.String == req.DurableWorkDir {
		writeJSON(w, 200, protocol.ResourceReleaseAcknowledgement{Recorded: true, DurableWorkDir: task.DurableWorkDir.String})
		return
	}
	workspace, daemonID, ok := h.resourceReleaseIdentity(w, r, req.ResourceReleaseRequest)
	if !ok {
		return
	}
	decision, err := h.TaskService.ResourceRelease(r.Context(), workspace, daemonID, req.ResourceReleaseRequest)
	if err != nil {
		writeError(w, 500, "resource dependencies unavailable")
		return
	}
	if !decision.ReleaseAllowed || (decision.ControlRequired && !decision.AdmissionClosed) {
		writeError(w, 409, "resource is still protected")
		return
	}
	if req.WorkDir == "" || req.WorkDir != task.WorkDir.String {
		// A historical frozen directory may have been removed without touching the
		// task's newer work directory. Its UI location must remain unchanged.
		writeJSON(w, 200, protocol.ResourceReleaseAcknowledgement{Recorded: false})
		return
	}
	updated, err := h.Queries.RecordReleasedTaskWorkDir(r.Context(), db.RecordReleasedTaskWorkDirParams{TaskID: task.ID, WorkDir: pgtype.Text{String: req.WorkDir, Valid: true}, DurableWorkDir: pgtype.Text{String: req.DurableWorkDir, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 409, "resource receipt conflicts with task location")
		return
	}
	if err != nil {
		writeError(w, 500, "record resource location failed")
		return
	}
	writeJSON(w, 200, protocol.ResourceReleaseAcknowledgement{Recorded: true, DurableWorkDir: updated.DurableWorkDir.String})
}

func (h *Handler) taskResourceGCProjection(r *http.Request, task db.AgentTaskQueue) (map[string]any, error) {
	rt, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
	if err != nil {
		return nil, err
	}
	addr := service.TaskResourceAddress(task, rt)
	turn, err := service.VerifiedDelegationResult(r.Context(), h.Queries, task)
	if err != nil {
		return nil, err
	}
	if turn != nil && turn.ScopeTaskID != "" {
		addr.ScopeType, addr.ScopeID = "task", turn.ScopeTaskID
	}
	decision, err := h.TaskService.ResourceRelease(r.Context(), rt.WorkspaceID, rt.DaemonID.String, protocol.ResourceReleaseRequest{Kind: "task", SourceTaskID: uuidToString(task.ID)})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": task.Status, "completed_at": timestampToPtr(task.CompletedAt), "resource_control_required": task.ResourceControlRequired, "terminal_reported_at": timestampToPtr(task.TerminalReportedAt), "resource_identity": addr, "resources_retained": !decision.ReleaseAllowed, "retention_reason": decision.Reason, "protected_resources": decision.ProtectedResources}, nil
}
