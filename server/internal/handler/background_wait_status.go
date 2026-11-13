package handler

import (
	"bytes"
	"encoding/json"
	"io"
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
	data, _ := json.Marshal(protocol.WaitAuthorization{DaemonID: r.URL.Query().Get("daemon_id")})
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(data))
	h.AuthorizeBackgroundWait(w, clone)
}

// BackgroundWaitStatuses is a batch receipt read. It does not lease, deliver,
// acknowledge, consume, or create any mailbox entry or managed obligation.
func (h *Handler) BackgroundWaitStatuses(w http.ResponseWriter, r *http.Request) {
	var req protocol.WaitDeliveryRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 262144)).Decode(&req) != nil || len(req.Waits) > 200 {
		writeError(w, 400, "at most 200 wait addresses required")
		return
	}
	ws := h.resolveWorkspaceID(r)
	if daemonWS := middleware.DaemonWorkspaceIDFromContext(r.Context()); daemonWS != "" {
		ws = daemonWS
	}
	if _, ok := parseUUIDOrBadRequest(w, ws, "workspace_id"); !ok {
		return
	}
	keys := make([]string, 0, len(req.Waits))
	seen := map[string]bool{}
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
		if !h.authorizeWaitLookup(w, r, a) {
			return
		}
		keys = append(keys, "background_wait:"+lookup.ID)
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
	out := protocol.WaitDeliveryResponse{Statuses: map[string]protocol.WaitDeliveryStatus{}, ObservedAt: time.Now().UTC()}
	for _, lookup := range req.Waits {
		status := protocol.WaitDeliveryStatus{Known: true}
		if row, ok := byKey["background_wait:"+lookup.ID]; ok {
			var result protocol.WaitResult
			a := lookup.Address
			if json.Unmarshal(row.Payload, &result) != nil || result.ID != lookup.ID || !sameWaitTarget(result.Address, a) || uuidToString(row.RecipientAgentID) != a.AgentID || (a.TaskID == "" && uuidToString(row.TargetSessionID) != a.SessionID) || (a.TaskID != "" && row.TargetSessionID.Valid) {
				writeError(w, 404, "wait receipt does not match address")
				return
			}
			status.Enqueued = true
			status.DeliveryID = uuidToString(row.ID)
			status.DeliveryStatus = row.DeliveryStatus
			status.ProcessingStatus = row.ProcessingStatus
		}
		out.Statuses[lookup.ID] = status
	}
	writeJSON(w, 200, out)
}

func sameWaitTarget(a, b protocol.WaitAddress) bool {
	return a.WorkspaceID == b.WorkspaceID && a.AgentID == b.AgentID && a.SessionID == b.SessionID && a.DaemonID == b.DaemonID && a.TaskID == b.TaskID && a.RuntimeID == b.RuntimeID
}

func (h *Handler) authorizeWaitLookup(w http.ResponseWriter, r *http.Request, a protocol.WaitAddress) bool {
	for field, value := range map[string]string{"agent_id": a.AgentID, "workspace_id": a.WorkspaceID} {
		if _, ok := parseUUIDOrBadRequest(w, value, field); !ok {
			return false
		}
	}
	source := r.Header.Get("X-Actor-Source")
	daemon := middleware.DaemonAuthPathFromContext(r.Context()) != ""
	if a.TaskID != "" {
		if _, ok := parseUUIDOrBadRequest(w, a.TaskID, "task_id"); !ok {
			return false
		}
		if !daemon && (source != "task_token" || r.Header.Get("X-Task-ID") != a.TaskID || r.Header.Get("X-Agent-ID") != a.AgentID) {
			writeError(w, 403, "current task required")
			return false
		}
		task, err := h.Queries.GetAgentTask(r.Context(), parseUUID(a.TaskID))
		if err != nil || uuidToString(task.AgentID) != a.AgentID || uuidToString(task.RuntimeID) != a.RuntimeID || task.SessionID.String != a.SessionID {
			writeError(w, 404, "task address unavailable")
			return false
		}
		if err := delegation.ValidateTaskScope(r.Context(), h.Queries, parseUUID(a.WorkspaceID), task); err != nil {
			writeError(w, 404, "task workspace unavailable")
			return false
		}
		rt, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
		if err != nil || uuidToString(rt.WorkspaceID) != a.WorkspaceID || rt.DaemonID.String != a.DaemonID {
			writeError(w, 404, "task daemon unavailable")
			return false
		}
		if daemon {
			return h.requireDaemonRuntimeAuthorized(w, r, a.WorkspaceID, rt.DaemonID, rt.OwnerID, "task unavailable")
		}
		return true
	}
	if _, ok := parseUUIDOrBadRequest(w, a.SessionID, "session_id"); !ok {
		return false
	}
	if !daemon {
		session, ok := externalActorSessionID(r)
		if source != "external_agent" || !ok || uuidToString(session) != a.SessionID || r.Header.Get("X-Agent-ID") != a.AgentID {
			writeError(w, 403, "own external session required")
			return false
		}
	}
	session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{ID: parseUUID(a.SessionID), WorkspaceID: parseUUID(a.WorkspaceID)})
	if err != nil || uuidToString(session.AgentID) != a.AgentID {
		writeError(w, 404, "session unavailable")
		return false
	}
	if daemon {
		return h.requireDaemonRuntimeAuthorized(w, r, a.WorkspaceID, pgtype.Text{String: a.DaemonID, Valid: a.DaemonID != ""}, session.OwnerUserID, "session unavailable")
	}
	if uuidToString(session.OwnerUserID) != requestUserID(r) {
		writeError(w, 403, "session owner required")
		return false
	}
	return true
}
