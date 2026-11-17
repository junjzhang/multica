package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// GetBackgroundWaitScope resolves only the credential's own external session
// or active task. It never registers a continuation.
func (h *Handler) GetBackgroundWaitScope(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") == "task_token" {
		ws, agent, taskID := h.resolveWorkspaceID(r), r.Header.Get("X-Agent-ID"), r.Header.Get("X-Task-ID")
		task, err := h.Queries.GetAgentTask(r.Context(), parseUUID(taskID))
		if err != nil || uuidToString(task.AgentID) != agent || task.Status != "running" {
			writeError(w, 403, "current task unavailable")
			return
		}
		if err := delegation.ValidateTaskScope(r.Context(), h.Queries, parseUUID(ws), task); err != nil {
			writeError(w, 403, "task scope unavailable")
			return
		}
		rt, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
		if err != nil || rt.DaemonID.String != r.URL.Query().Get("daemon_id") || uuidToString(rt.WorkspaceID) != ws {
			writeError(w, 403, "task daemon unavailable")
			return
		}
		writeJSON(w, 200, protocol.WaitAddress{WorkspaceID: ws, AgentID: agent, TaskID: taskID, DaemonID: rt.DaemonID.String})
		return
	}
	if !isExternalAgentActor(r) {
		writeError(w, 403, "session-bound external credential required")
		return
	}
	address, ok := h.externalWaitScope(w, r, r.URL.Query().Get("daemon_id"), false)
	if ok {
		writeJSON(w, 200, address)
	}

}

// BackgroundWaitStatuses reads each authorized receipt independently. An
// unavailable historical resource produces unknown for that entry, without
// discarding unrelated receipts. Credential scope violations reject the request.
func (h *Handler) BackgroundWaitStatuses(w http.ResponseWriter, r *http.Request) {
	var req protocol.WaitDeliveryRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 262144)).Decode(&req) != nil || len(req.Waits) > 200 {
		writeError(w, 400, "at most 200 wait addresses required")
		return
	}
	ws := h.resolveWorkspaceID(r)
	daemon := middleware.DaemonAuthPathFromContext(r.Context()) != ""
	if daemonWS := middleware.DaemonWorkspaceIDFromContext(r.Context()); daemonWS != "" {
		ws = daemonWS
	}
	if _, ok := parseUUIDOrBadRequest(w, ws, "workspace_id"); !ok {
		return
	}
	source := r.Header.Get("X-Actor-Source")
	if !daemon && source != "external_agent" && source != "task_token" {
		writeError(w, 403, "wait reader credential required")
		return
	}
	out := protocol.WaitDeliveryResponse{Statuses: map[string]protocol.WaitDeliveryStatus{}}
	keys := make([]string, 0, len(req.Waits))
	seen := map[string]bool{}
	scopes := map[protocol.WaitAddress]bool{}
	for _, lookup := range req.Waits {
		if _, ok := parseUUIDOrBadRequest(w, lookup.ID, "wait_id"); !ok {
			return
		}
		if seen[lookup.ID] {
			writeError(w, 400, "duplicate wait id")
			return
		}
		seen[lookup.ID] = true
		a := lookup.Address
		if a.WorkspaceID != ws {
			writeError(w, 403, "wait outside workspace")
			return
		}
		if _, ok := parseUUIDOrBadRequest(w, a.AgentID, "agent_id"); !ok {
			return
		}
		if a.TaskID != "" {
			if _, ok := parseUUIDOrBadRequest(w, a.TaskID, "task_id"); !ok {
				return
			}
		} else {
			if _, ok := parseUUIDOrBadRequest(w, a.SessionID, "session_id"); !ok {
				return
			}
		}
		if !daemon {
			if source == "task_token" {
				if a.TaskID == "" || a.TaskID != r.Header.Get("X-Task-ID") || a.AgentID != r.Header.Get("X-Agent-ID") {
					writeError(w, 403, "current task required")
					return
				}
			} else {
				session, ok := externalActorSessionID(r)
				if a.TaskID != "" || !ok || a.SessionID != uuidToString(session) || a.AgentID != r.Header.Get("X-Agent-ID") {
					writeError(w, 403, "own external session required")
					return
				}
			}
		}
		// Display metadata is not authority, and does not split identical scopes.
		scope := a
		scope.WorkDir, scope.IssueID = "", ""
		allowed, checked := scopes[scope]
		if !checked {
			allowed = h.canReadWaitLookup(r, a, daemon)
			scopes[scope] = allowed
		}
		out.Statuses[lookup.ID] = protocol.WaitDeliveryStatus{Known: allowed}
		if allowed {
			keys = append(keys, "background_wait:"+lookup.ID)
		}
	}
	rows, err := h.Queries.GetBackgroundWaitDeliveries(r.Context(), db.GetBackgroundWaitDeliveriesParams{WorkspaceID: parseUUID(ws), NotificationKeys: keys})
	if err != nil {
		writeError(w, 500, "cannot read wait receipts")
		return
	}
	byKey := map[string]db.AgentMailboxMessage{}
	for _, row := range rows {
		byKey[row.NotificationKey.String] = row
	}
	for _, lookup := range req.Waits {
		status := out.Statuses[lookup.ID]
		if !status.Known {
			continue
		}
		if row, ok := byKey["background_wait:"+lookup.ID]; ok {
			var result protocol.WaitResult
			a := lookup.Address
			if json.Unmarshal(row.Payload, &result) != nil || result.ID != lookup.ID || !sameWaitTarget(result.Address, a) || uuidToString(row.RecipientAgentID) != a.AgentID || (a.TaskID == "" && uuidToString(row.TargetSessionID) != a.SessionID) || (a.TaskID != "" && row.TargetSessionID.Valid) {
				out.Statuses[lookup.ID] = protocol.WaitDeliveryStatus{}
				continue
			}
			status.Enqueued = true
			status.DeliveryID = uuidToString(row.ID)
			status.DeliveryStatus = row.DeliveryStatus
			status.ProcessingStatus = row.ProcessingStatus
		}
		out.Statuses[lookup.ID] = status
	}
	out.ObservedAt = time.Now().UTC()
	writeJSON(w, 200, out)
}

func sameWaitTarget(a, b protocol.WaitAddress) bool {
	return a.WorkspaceID == b.WorkspaceID && a.AgentID == b.AgentID && a.SessionID == b.SessionID && a.DaemonID == b.DaemonID && a.TaskID == b.TaskID && a.RuntimeID == b.RuntimeID
}

// Inputs have passed UUID validation and credential scope checks above. Failure
// reveals no receipt information, including whether a mailbox row exists.
func (h *Handler) canReadWaitLookup(r *http.Request, a protocol.WaitAddress, daemon bool) bool {
	if a.TaskID != "" {
		task, err := h.Queries.GetAgentTask(r.Context(), parseUUID(a.TaskID))
		// A consumed wait can outlive the task's provider pin. The immutable mailbox
		// payload, rather than the current pin, verifies its historical session.
		if err != nil || uuidToString(task.AgentID) != a.AgentID || uuidToString(task.RuntimeID) != a.RuntimeID {
			return false
		}
		if delegation.ValidateTaskScope(r.Context(), h.Queries, parseUUID(a.WorkspaceID), task) != nil {
			return false
		}
		rt, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
		if err != nil || uuidToString(rt.WorkspaceID) != a.WorkspaceID || rt.DaemonID.String != a.DaemonID {
			return false
		}
		return !daemon || h.verifyDaemonRuntimeAuthorized(r, rt)
	}
	session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{ID: parseUUID(a.SessionID), WorkspaceID: parseUUID(a.WorkspaceID)})
	if err != nil || uuidToString(session.AgentID) != a.AgentID {
		return false
	}
	if daemon {
		rt := db.AgentRuntime{WorkspaceID: session.WorkspaceID, DaemonID: pgtype.Text{String: a.DaemonID, Valid: a.DaemonID != ""}, OwnerID: session.OwnerUserID}
		if !h.verifyDaemonRuntimeAuthorized(r, rt) {
			return false
		}
	} else if uuidToString(session.OwnerUserID) != requestUserID(r) {
		return false
	}
	owned, err := h.Queries.WorkspaceDaemonOwnedByUser(r.Context(), db.WorkspaceDaemonOwnedByUserParams{WorkspaceID: session.WorkspaceID, DaemonID: pgtype.Text{String: a.DaemonID, Valid: a.DaemonID != ""}, OwnerUserID: session.OwnerUserID})
	return err == nil && owned
}
