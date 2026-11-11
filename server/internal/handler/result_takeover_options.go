package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ResultTakeoverTargetDisplay struct {
	Provider    string `json:"provider"`
	SessionRef  string `json:"session_ref"`
	DisplayName string `json:"display_name"`
}

type ResultTakeoverTargetOption struct {
	resultTakeoverTarget
	ResultTakeoverTargetDisplay
	Available bool `json:"available"`
}

type ResultTakeoverReturnView struct {
	ReturnID string                       `json:"return_id"`
	Kind     string                       `json:"kind"`
	Target   *ResultTakeoverTargetDisplay `json:"target"`
}

type ResultTakeoverOptionsResponse struct {
	CurrentReturnID string                       `json:"current_return_id"`
	Returns         []ResultTakeoverReturnView   `json:"returns"`
	Targets         []ResultTakeoverTargetOption `json:"targets"`
}

func (h *Handler) GetResultTakeoverOptions(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "result takeover requires a human actor")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	result, ok := parseUUIDOrBadRequest(w, r.URL.Query().Get("result_task_id"), "result_task_id")
	if !ok {
		return
	}
	from, ok := parseUUIDOrBadRequest(w, r.URL.Query().Get("from_return_id"), "from_return_id")
	if !ok {
		return
	}
	actor, ok := parseUUIDOrBadRequest(w, requestUserID(r), "user_id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "load takeover options failed")
		return
	}
	defer tx.Rollback(r.Context())
	// All displayed versions and candidate proofs describe the same snapshot.
	// Only SELECTs and the same admission share locks as POST run here.
	if _, err = tx.Exec(r.Context(), "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		writeError(w, 500, "load takeover options failed")
		return
	}
	q := h.Queries.WithTx(tx)
	if _, err = q.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 404, "issue not found")
		} else {
			writeError(w, 500, "load takeover options failed")
		}
		return
	}
	prior, err := q.GetDelegationReturnByID(r.Context(), from)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (prior.WorkspaceID != issue.WorkspaceID || prior.IssueID != issue.ID) {
		writeError(w, 404, "result return not found")
		return
	}
	if err != nil {
		writeError(w, 500, "load takeover options failed")
		return
	}
	agent, err := q.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: prior.DispatcherAgentID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "dispatcher not found")
		return
	}
	if err != nil {
		writeError(w, 500, "load takeover options failed")
		return
	}
	snapshot := *h
	snapshot.Queries = q
	if !snapshot.canManageAgent(w, r, agent) {
		return
	}
	in := takeoverInput{workspace: issue.WorkspaceID, issue: issue.ID, actor: actor, dispatcher: agent.ID, result: result, from: from}
	outcome, err := q.GetAgentTask(r.Context(), in.result)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "saved result not found")
		return
	}
	if err != nil {
		writeError(w, 500, "load takeover options failed")
		return
	}
	if err = validateTakeoverSavedOutcome(r.Context(), q, in, outcome); err != nil {
		var problem takeoverError
		if errors.As(err, &problem) {
			writeError(w, problem.status, problem.message)
		} else {
			writeError(w, 500, "load takeover options failed")
		}
		return
	}
	response, err := h.readTakeoverOptions(r.Context(), q, in, agent)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "current return is unavailable")
		return
	}
	if err != nil {
		writeError(w, 500, "load takeover options failed")
		return
	}
	writeJSON(w, 200, response)
}

func (h *Handler) readTakeoverOptions(ctx context.Context, q *db.Queries, in takeoverInput, agent db.Agent) (ResultTakeoverOptionsResponse, error) {
	out := ResultTakeoverOptionsResponse{Returns: []ResultTakeoverReturnView{}, Targets: []ResultTakeoverTargetOption{}}
	current, err := q.GetIssueDelegationReturnForDispatcher(ctx, db.GetIssueDelegationReturnForDispatcherParams{WorkspaceID: in.workspace, IssueID: in.issue, DispatcherAgentID: in.dispatcher})
	if err != nil {
		return out, err
	}
	out.CurrentReturnID = uuidToString(current.ID)
	versions, err := q.ListIssueDelegationReturns(ctx, db.ListIssueDelegationReturnsParams{WorkspaceID: in.workspace, IssueID: in.issue})
	if err != nil {
		return out, err
	}

	var selectors []pgtype.UUID
	switch agent.ExecutionKind {
	case "external":
		in.kind = delegation.KindExternalSession
		sessions, err := q.ListExternalAgentSessionsForRead(ctx, db.ListExternalAgentSessionsForReadParams{WorkspaceID: in.workspace, AgentID: in.dispatcher})
		if err != nil {
			return out, err
		}
		for _, session := range sessions {
			selectors = append(selectors, session.ID)
		}
	case "managed":
		in.kind = delegation.KindManagedTask
		sources, err := q.ListResultTakeoverManagedSources(ctx, db.ListResultTakeoverManagedSourcesParams{WorkspaceID: in.workspace, AgentID: in.dispatcher})
		if err != nil {
			return out, err
		}
		for _, source := range sources {
			selectors = append(selectors, source.ID)
		}
	}
	for _, selector := range selectors {
		target, source, err := delegation.PrepareTakeoverTarget(ctx, q, in.workspace, in.issue, in.dispatcher, in.kind, selector)
		if err == nil {
			err = h.authorizeTakeoverTarget(ctx, q, in, target, source)
		}
		if unavailableTakeoverTarget(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		display, err := takeoverTargetDisplay(ctx, q, target)
		if err != nil {
			return out, err
		}
		option := ResultTakeoverTargetOption{resultTakeoverTarget: resultTakeoverTarget{Kind: in.kind}, ResultTakeoverTargetDisplay: *display, Available: true}
		if in.kind == delegation.KindExternalSession {
			option.ExternalSessionID = uuidToString(selector)
		} else {
			option.SourceTaskID = uuidToString(selector)
			option.SelectionVersion = delegation.TakeoverSelectionVersion(target, source)
		}
		out.Targets = append(out.Targets, option)
	}
	for _, version := range versions {
		if version.DispatcherAgentID != in.dispatcher {
			continue
		}
		display, err := h.readTakeoverReturnDisplay(ctx, q, in, version)
		if err != nil && !unavailableTakeoverTarget(err) {
			return out, err
		}
		out.Returns = append(out.Returns, ResultTakeoverReturnView{ReturnID: uuidToString(version.ID), Kind: version.ReturnKind, Target: display})
	}
	return out, nil
}

// Missing or inaccessible historical scope hides target details, not the
// already visible return identity. Storage errors remain errors, never [].
func (h *Handler) readTakeoverReturnDisplay(ctx context.Context, q *db.Queries, in takeoverInput, address db.IssueDelegationReturn) (*ResultTakeoverTargetDisplay, error) {
	in.kind = delegation.Kind(address.ReturnKind)
	var source db.AgentTaskQueue
	if in.kind == delegation.KindManagedTask {
		var err error
		source, err = q.GetAgentTask(ctx, address.SourceTaskID)
		if err != nil {
			return nil, err
		}
		if source.AgentID != address.DispatcherAgentID || source.RuntimeID != address.RuntimeID {
			return nil, delegation.ErrReturnInvalid
		}
		if address.ScopeTaskID.Valid {
			if err := delegation.ValidateOriginalTaskScope(ctx, q, address); err != nil {
				return nil, err
			}
		} else if source.IssueID.Valid {
			if _, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: source.IssueID, WorkspaceID: in.workspace}); err != nil {
				return nil, err
			}
		}
	}
	if err := h.authorizeTakeoverTarget(ctx, q, in, address, source); err != nil {
		return nil, err
	}
	return takeoverTargetDisplay(ctx, q, address)
}

func takeoverTargetDisplay(ctx context.Context, q *db.Queries, address db.IssueDelegationReturn) (*ResultTakeoverTargetDisplay, error) {
	if address.ReturnKind == string(delegation.KindExternalSession) {
		session, err := q.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{ID: address.ExternalSessionID, WorkspaceID: address.WorkspaceID})
		if err != nil {
			return nil, err
		}
		name := session.DisplayName
		if name == "" {
			name = "External session"
		}
		return &ResultTakeoverTargetDisplay{Provider: session.Provider, SessionRef: takeoverSessionRef(session.Provider, session.SessionID), DisplayName: name}, nil
	}
	source, err := q.GetAgentTask(ctx, address.SourceTaskID)
	if err != nil {
		return nil, err
	}
	name, err := takeoverScopeLabel(ctx, q, address, source)
	if err != nil {
		return nil, err
	}
	return &ResultTakeoverTargetDisplay{Provider: address.Provider.String, SessionRef: takeoverSessionRef(address.Provider.String, address.SessionID.String), DisplayName: name}, nil
}

// Scope labels are read only after the caller's scope-access check. Neither
// provider session paths nor task prompts are presentation fallbacks.
func takeoverScopeLabel(ctx context.Context, q *db.Queries, address db.IssueDelegationReturn, source db.AgentTaskQueue) (string, error) {
	fallback := "Run"
	if source.CreatedAt.Valid {
		fallback += " · " + source.CreatedAt.Time.UTC().Format("2006-01-02 15:04 UTC")
	}
	if address.ScopeTaskID.Valid {
		return fallback, nil
	}
	if source.IssueID.Valid {
		issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: source.IssueID, WorkspaceID: address.WorkspaceID})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Issue #%d · %s", issue.Number, issue.Title), nil
	}
	if source.ChatSessionID.Valid {
		chat, err := q.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: source.ChatSessionID, WorkspaceID: address.WorkspaceID})
		if err != nil {
			return "", err
		}
		if chat.Title != "" {
			return "Chat · " + chat.Title, nil
		}
		return "Chat", nil
	}
	if source.AutopilotRunID.Valid {
		run, err := q.GetAutopilotRun(ctx, source.AutopilotRunID)
		if err != nil {
			return "", err
		}
		autopilot, err := q.GetAutopilotInWorkspace(ctx, db.GetAutopilotInWorkspaceParams{ID: run.AutopilotID, WorkspaceID: address.WorkspaceID})
		if err != nil {
			return "", err
		}
		return "Autopilot · " + autopilot.Title, nil
	}
	return fallback, nil
}

func takeoverSessionRef(provider, sessionID string) string {
	digest := sha256.Sum256([]byte(provider + "\x00" + sessionID))
	return hex.EncodeToString(digest[:6])
}

func unavailableTakeoverTarget(err error) bool {
	if err == nil {
		return false
	}
	var denied takeoverError
	return errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &denied) && denied.status < 500) || errors.Is(err, delegation.ErrReturnInvalid) || errors.Is(err, delegation.ErrReturnNotReady) || errors.Is(err, delegation.ErrReturnSourceClosed) || errors.Is(err, delegation.ErrReturnConflict)
}
