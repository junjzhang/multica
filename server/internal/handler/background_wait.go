package handler

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// AuthorizeBackgroundWait resolves a return address, never schedules a command.
// Execution additionally requires a capability read from the local daemon's
// private profile directory. A session UUID or an API credential alone cannot
// submit executable work to a daemon.
func (h *Handler) AuthorizeBackgroundWait(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "external_agent" && r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "background waits require an external session and local daemon access")
		return
	}
	var req protocol.WaitAuthorization
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	wsID, agentID := h.resolveWorkspaceID(r), r.Header.Get("X-Agent-ID")
	if r.Header.Get("X-Actor-Source") == "task_token" {
		if _, ok := parseUUIDOrBadRequest(w, req.ID, "id"); !ok {
			return
		}
		if _, err := hex.DecodeString(req.RequestHash); err != nil || len(req.RequestHash) != 64 || req.Deadline.IsZero() {
			writeError(w, 400, "request hash and bounded deadline required")
			return
		}
		address, err := h.TaskService.RegisterManagedWait(r.Context(), parseUUID(wsID), parseUUID(agentID), parseUUID(r.Header.Get("X-Task-ID")), req)
		if err != nil {
			writeManagedWaitError(w, err)
			return
		}
		writeJSON(w, 200, address)
		return
	}
	// The credential's bound session is the wait's registered address. The
	// X-Return-Session header is only a consistency parameter and was already
	// compared against it by the auth middleware.
	sessionUUID, ok := h.requireExternalActorSession(w, r)
	if !ok {
		return
	}
	address, err := delegation.ResolveExternal(r.Context(), h.Queries, wsID, agentID, uuidToString(sessionUUID))
	if err != nil {
		writeError(w, http.StatusForbidden, "return session is unavailable")
		return
	}
	session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{ID: parseUUID(address.ExternalSessionID), WorkspaceID: parseUUID(wsID)})
	if err != nil || session.Provider != "paseo" || session.DaemonID != req.DaemonID || uuidToString(session.OwnerUserID) != requestUserID(r) {
		writeError(w, http.StatusForbidden, "session does not belong to this daemon owner")
		return
	}
	owned, err := h.Queries.WorkspaceDaemonOwnedByUser(r.Context(), db.WorkspaceDaemonOwnedByUserParams{WorkspaceID: session.WorkspaceID, DaemonID: pgtype.Text{String: session.DaemonID, Valid: true}, OwnerUserID: session.OwnerUserID})
	if err != nil || !owned {
		writeError(w, http.StatusForbidden, "daemon is not owned by the session owner")
		return
	}
	writeJSON(w, http.StatusOK, protocol.WaitAddress{WorkspaceID: wsID, AgentID: agentID, SessionID: address.ExternalSessionID, DaemonID: session.DaemonID})
}

// CompleteBackgroundWait is an idempotent outbox sink. The daemon commits its
// terminal journal before calling it, and replays the same immutable result
// after a crash or a lost response. Only the executing daemon's owner can write.
func (h *Handler) CompleteBackgroundWait(w http.ResponseWriter, r *http.Request) {
	var req protocol.WaitResult
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid result")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, req.ID, "id"); !ok {
		return
	}
	if req.Address.DaemonID == "" {
		writeError(w, http.StatusBadRequest, "daemon_id is required")
		return
	}
	if req.Address.TaskID != "" {
		if _, ok := parseUUIDOrBadRequest(w, req.Address.TaskID, "task_id"); !ok {
			return
		}
		task, workspace, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, req.Address.TaskID)
		if !ok {
			return
		}
		if workspace != req.Address.WorkspaceID || uuidToString(task.RuntimeID) != req.Address.RuntimeID || !protocol.WaitTerminal(req.State) || req.FinishedAt.IsZero() || req.FinishedAt.After(time.Now().Add(time.Minute)) || len(req.LogPath) > 4096 {
			writeError(w, 400, "invalid managed wait result")
			return
		}
		runtime, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
		if err != nil || runtime.DaemonID.String != req.Address.DaemonID {
			writeError(w, 404, "wait daemon not found")
			return
		}
		id, err := h.TaskService.CompleteManagedWait(r.Context(), req)
		if err != nil {
			writeManagedWaitError(w, err)
			return
		}
		if task.Status == "cancelled" && !task.StartedAt.Valid {
			_, _ = h.Queries.RecordTaskTerminalReport(r.Context(), task.ID)
		}
		h.TaskService.NotifyTaskEnqueued(r.Context(), task)
		writeJSON(w, 200, map[string]string{"delivery_id": id})
		return
	}
	sessionID, ok := parseUUIDOrBadRequest(w, req.Address.SessionID, "session_id")
	if !ok {
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, req.Address.WorkspaceID, "workspace_id")
	if !ok {
		return
	}
	if !protocol.WaitTerminal(req.State) || req.FinishedAt.IsZero() || req.FinishedAt.After(time.Now().Add(time.Minute)) || len(req.LogPath) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid terminal result")
		return
	}
	session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{ID: sessionID, WorkspaceID: wsID})
	if err != nil || uuidToString(session.AgentID) != req.Address.AgentID {
		writeError(w, http.StatusNotFound, "wait session not found")
		return
	}
	// Qualify the executing daemon, not the session's mutable delivery worker.
	daemonID := pgtype.Text{String: req.Address.DaemonID, Valid: req.Address.DaemonID != ""}
	if !h.requireDaemonRuntimeAuthorized(w, r, req.Address.WorkspaceID, daemonID, session.OwnerUserID, "wait session not found") {
		return
	}
	owned, err := h.Queries.WorkspaceDaemonOwnedByUser(r.Context(), db.WorkspaceDaemonOwnedByUserParams{WorkspaceID: wsID, DaemonID: daemonID, OwnerUserID: session.OwnerUserID})
	if err != nil || !owned {
		writeError(w, http.StatusNotFound, "wait daemon not found")
		return
	}
	payload, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid result")
		return
	}
	key := "background_wait:" + req.ID
	message, err := delivery.NewPostgres(h.Queries).Enqueue(r.Context(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: req.Address.WorkspaceID},
		Source:          delivery.Source{Type: "background_wait", Key: key},
		Target:          delivery.Target{SessionID: req.Address.SessionID},
		Recipient:       delivery.Recipient{Type: "agent", ID: req.Address.AgentID},
		NotificationKey: key, Kind: "background_wait", Payload: payload,
	})
	if errors.Is(err, delivery.ErrConflict) {
		writeError(w, http.StatusConflict, "wait result conflicts with its accepted result")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue wait result")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"delivery_id": message.ID})
}

func (h *Handler) ReadBackgroundWait(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, 403, "current task credential required")
		return
	}
	id := chi.URLParam(r, "waitId")
	if _, ok := parseUUIDOrBadRequest(w, id, "wait_id"); !ok {
		return
	}
	consume := strings.HasSuffix(r.URL.Path, "/read")
	wait, err := h.TaskService.ReadManagedWait(r.Context(), parseUUID(h.resolveWorkspaceID(r)), parseUUID(r.Header.Get("X-Agent-ID")), parseUUID(r.Header.Get("X-Task-ID")), id, consume)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, service.ErrManagedWaitConflict) || errors.Is(err, delegation.ErrReturnInvalid) {
			writeError(w, 404, "wait not found in current task")
		} else {
			writeManagedWaitError(w, err)
		}
		return
	}
	status := protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: wait.ID, Address: wait.Address, State: "running"}, Deadline: wait.Deadline, DeliveryID: wait.DeliveryID, Consumed: wait.Consumed}
	if wait.Result != nil {
		status.WaitResult = *wait.Result
	}
	writeJSON(w, 200, status)
}

func writeManagedWaitError(w http.ResponseWriter, err error) {
	if errors.Is(err, delegation.ErrReturnInvalid) || errors.Is(err, delegation.ErrReturnSourceClosed) {
		writeError(w, 403, "managed task scope unavailable")
		return
	}
	if errors.Is(err, service.ErrManagedWaitConflict) || errors.Is(err, delegation.ErrReturnNotReady) {
		writeError(w, 409, "managed wait requires its registered current task and confirmed session")
		return
	}
	writeError(w, 500, "managed wait state could not be committed; retry the same request")
}

func validTaskTurnRequest(w http.ResponseWriter, turn protocol.TaskTurn) bool {
	if turn.Generation != "" {
		if _, ok := parseUUIDOrBadRequest(w, turn.Generation, "turn_generation"); !ok {
			return false
		}
	}
	for _, id := range turn.KnownWaitIDs {
		if _, ok := parseUUIDOrBadRequest(w, id, "known_wait_ids"); !ok {
			return false
		}
	}
	return true
}
