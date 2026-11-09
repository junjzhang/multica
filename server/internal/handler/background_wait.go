package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/delivery"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// AuthorizeBackgroundWait resolves a return address, never schedules a command.
// Execution additionally requires a capability read from the local daemon's
// private profile directory. A session UUID or an API credential alone cannot
// submit executable work to a daemon.
func (h *Handler) AuthorizeBackgroundWait(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "external_agent" {
		writeError(w, http.StatusForbidden, "background waits require an external session and local daemon access")
		return
	}
	var req struct {
		DaemonID string `json:"daemon_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	wsID, agentID := h.resolveWorkspaceID(r), r.Header.Get("X-Agent-ID")
	address, err := delegation.ResolveExternal(r.Context(), h.Queries, wsID, agentID, r.Header.Get(ReturnSessionHeader))
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
