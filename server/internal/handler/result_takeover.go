package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type resultTakeoverTarget struct {
	Kind              delegation.Kind `json:"kind"`
	ExternalSessionID string          `json:"external_session_id,omitempty"`
	SourceTaskID      string          `json:"source_task_id,omitempty"`
}

type resultTakeoverRequest struct {
	ClientRequestID         string               `json:"client_request_id"`
	ResultTaskID            string               `json:"result_task_id"`
	FromReturnID            string               `json:"from_return_id"`
	ExpectedCurrentReturnID string               `json:"expected_current_return_id"`
	Target                  resultTakeoverTarget `json:"target"`
}

type ResultTakeoverResponse struct {
	ID              string `json:"id"`
	ClientRequestID string `json:"client_request_id"`
	ResultTaskID    string `json:"result_task_id"`
	FromReturnID    string `json:"from_return_id"`
	TargetReturnID  string `json:"target_return_id"`
	CurrentReturnID string `json:"current_return_id"`
	CreatedAt       string `json:"created_at"`
}

type takeoverInput struct {
	workspace, issue, actor, dispatcher       pgtype.UUID
	request, result, from, expected, selector pgtype.UUID
	kind                                      delegation.Kind
}

type takeoverError struct {
	status  int
	message string
}

func (e takeoverError) Error() string { return e.message }

func (h *Handler) TakeoverDelegationResult(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "result takeover requires a human actor")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	actor, ok := parseUUIDOrBadRequest(w, requestUserID(r), "user_id")
	if !ok {
		return
	}
	var body resultTakeoverRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeError(w, 400, "invalid result takeover request")
		return
	}
	input := takeoverInput{workspace: issue.WorkspaceID, issue: issue.ID, actor: actor, kind: body.Target.Kind}
	for _, field := range []struct {
		value, name string
		dest        *pgtype.UUID
	}{
		{body.ClientRequestID, "client_request_id", &input.request}, {body.ResultTaskID, "result_task_id", &input.result},
		{body.FromReturnID, "from_return_id", &input.from}, {body.ExpectedCurrentReturnID, "expected_current_return_id", &input.expected},
	} {
		id, ok := parseUUIDOrBadRequest(w, field.value, field.name)
		if !ok {
			return
		}
		*field.dest = id
	}
	selector := ""
	switch input.kind {
	case delegation.KindExternalSession:
		if body.Target.SourceTaskID != "" {
			writeError(w, 400, "external target cannot name a managed task")
			return
		}
		selector = body.Target.ExternalSessionID
	case delegation.KindManagedTask:
		if body.Target.ExternalSessionID != "" {
			writeError(w, 400, "managed target cannot name an external session")
			return
		}
		selector = body.Target.SourceTaskID
	default:
		writeError(w, 400, "invalid result takeover target kind")
		return
	}
	input.selector, ok = parseUUIDOrBadRequest(w, selector, "target selector")
	if !ok {
		return
	}
	prior, err := h.Queries.GetDelegationReturnByID(r.Context(), input.from)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (prior.WorkspaceID != issue.WorkspaceID || prior.IssueID != issue.ID) {
		writeError(w, 404, "result return not found")
		return
	}
	if err != nil {
		writeError(w, 500, "load result return failed")
		return
	}
	input.dispatcher = prior.DispatcherAgentID
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: input.dispatcher, WorkspaceID: input.workspace})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "dispatcher not found")
		return
	}
	if err != nil {
		writeError(w, 500, "load dispatcher failed")
		return
	}
	if !h.canManageAgent(w, r, agent) {
		return
	}
	action, current, created, err := h.takeoverResult(r.Context(), input)
	if err != nil {
		var problem takeoverError
		switch {
		case errors.As(err, &problem):
			writeError(w, problem.status, problem.message)
		case errors.Is(err, delegation.ErrReturnInvalid), errors.Is(err, delegation.ErrReturnNotReady), errors.Is(err, delegation.ErrReturnSourceClosed), errors.Is(err, delegation.ErrReturnConflict), errors.Is(err, sessionroute.ErrInvalidBinding):
			writeError(w, 409, err.Error())
		case isUniqueViolation(err):
			writeError(w, 409, "takeover request changed concurrently; retry the same request")
		default:
			writeError(w, 500, "result takeover failed")
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, ResultTakeoverResponse{ID: uuidToString(action.ID), ClientRequestID: uuidToString(action.ClientRequestID), ResultTaskID: uuidToString(action.ResultTaskID), FromReturnID: uuidToString(action.FromReturnID), TargetReturnID: uuidToString(action.TargetReturnID), CurrentReturnID: uuidToString(current), CreatedAt: action.CreatedAt.Time.UTC().Format(time.RFC3339Nano)})
}

func (h *Handler) takeoverResult(ctx context.Context, in takeoverInput) (db.DelegationResultTakeover, pgtype.UUID, bool, error) {
	var empty db.DelegationResultTakeover
	// A replay is an observation of accepted work, not another admission to a
	// source task that may already have ended.
	previous, err := h.Queries.GetResultTakeoverByRequest(ctx, db.GetResultTakeoverByRequestParams{WorkspaceID: in.workspace, ClientRequestID: in.request})
	if err == nil {
		return h.takeoverReplay(ctx, h.Queries, in, previous)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, pgtype.UUID{}, false, err
	}
	owners := []pgtype.UUID{in.issue}
	if in.kind == delegation.KindManagedTask {
		source, err := h.Queries.GetAgentTask(ctx, in.selector)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, pgtype.UUID{}, false, err
		}
		if source.IssueID.Valid {
			owners = append(owners, source.IssueID)
		}
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	lockedIssues, err := q.LockDelegationResultIssues(ctx, db.LockDelegationResultIssuesParams{WorkspaceID: in.workspace, IssueIds: owners})
	if err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	if !slices.ContainsFunc(lockedIssues, func(issue db.Issue) bool { return issue.ID == in.issue }) {
		return empty, pgtype.UUID{}, false, takeoverError{404, "delegated issue no longer exists"}
	}
	previous, err = q.GetResultTakeoverByRequest(ctx, db.GetResultTakeoverByRequestParams{WorkspaceID: in.workspace, ClientRequestID: in.request})
	if err == nil {
		return h.takeoverReplay(ctx, q, in, previous)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, pgtype.UUID{}, false, err
	}
	if err = validateTakeoverResult(ctx, q, in); err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	target, source, err := delegation.PrepareTakeoverTarget(ctx, q, in.workspace, in.issue, in.dispatcher, in.kind, in.selector)
	if err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	if source.IssueID.Valid && !slices.Contains(owners, source.IssueID) {
		return empty, pgtype.UUID{}, false, takeoverError{409, "target source scope changed; retry"}
	}
	if err = h.authorizeTakeoverTarget(ctx, q, in, target, source); err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	current, err := q.LockIssueDelegationReturnForDispatcher(ctx, db.LockIssueDelegationReturnForDispatcherParams{WorkspaceID: in.workspace, IssueID: in.issue, DispatcherAgentID: in.dispatcher})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, pgtype.UUID{}, false, takeoverError{409, "current return is unavailable"}
	}
	if err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	if current.ID != in.expected && current.ID != target.ID {
		return empty, pgtype.UUID{}, false, takeoverError{409, "current return changed; reload before taking over"}
	}
	if !target.ID.Valid {
		target, err = q.CreateDelegationReturnVersion(ctx, db.CreateDelegationReturnVersionParams{ID: dbid.NewV7(), WorkspaceID: target.WorkspaceID, IssueID: target.IssueID, DispatcherAgentID: target.DispatcherAgentID, ReturnKind: target.ReturnKind, ExternalSessionID: target.ExternalSessionID, SourceTaskID: target.SourceTaskID, RuntimeID: target.RuntimeID, Provider: target.Provider, SessionID: target.SessionID, WorkDir: target.WorkDir, ScopeTaskID: target.ScopeTaskID})
		if err != nil {
			return empty, pgtype.UUID{}, false, err
		}
	}
	if current.ID != target.ID {
		if err = q.RetireCurrentDelegationReturn(ctx, db.RetireCurrentDelegationReturnParams{WorkspaceID: in.workspace, IssueID: in.issue, DispatcherAgentID: in.dispatcher}); err != nil {
			return empty, pgtype.UUID{}, false, err
		}
		if err = q.SetCurrentDelegationReturn(ctx, target.ID); err != nil {
			return empty, pgtype.UUID{}, false, err
		}
	}
	if in.kind == delegation.KindExternalSession {
		_, err = sessionroute.Bind(ctx, q, sessionroute.BindRequest{WorkspaceID: uuidToString(in.workspace), AgentID: uuidToString(in.dispatcher), IssueID: uuidToString(in.issue), SessionID: uuidToString(target.ExternalSessionID), CreatedBy: uuidToString(in.actor)})
		if err != nil {
			return empty, pgtype.UUID{}, false, err
		}
	}
	action, err := q.CreateResultTakeover(ctx, db.CreateResultTakeoverParams{ID: dbid.NewV7(), WorkspaceID: in.workspace, ClientRequestID: in.request, ResultTaskID: in.result, FromReturnID: in.from, ExpectedCurrentReturnID: in.expected, TargetReturnID: target.ID, TargetKind: string(in.kind), TargetSelectorID: in.selector, CreatedBy: in.actor})
	if err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	_, err = q.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: in.result, DelegationReturnID: target.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.CreateTakeoverResultObligation(ctx, db.CreateTakeoverResultObligationParams{TaskID: in.result, DelegationReturnID: target.ID, TakeoverID: action.ID})
	}
	if err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, pgtype.UUID{}, false, err
	}
	if h.Bus != nil {
		h.Bus.Publish(events.Event{Type: protocol.EventIssueMailboxChanged, WorkspaceID: uuidToString(in.workspace), ActorType: "member", Payload: map[string]any{"issue_id": uuidToString(in.issue)}})
	}
	return action, target.ID, true, nil
}

func (h *Handler) takeoverReplay(ctx context.Context, q *db.Queries, in takeoverInput, action db.DelegationResultTakeover) (db.DelegationResultTakeover, pgtype.UUID, bool, error) {
	if action.CreatedBy != in.actor || action.ResultTaskID != in.result || action.FromReturnID != in.from || action.ExpectedCurrentReturnID != in.expected || action.TargetKind != string(in.kind) || action.TargetSelectorID != in.selector {
		return action, pgtype.UUID{}, false, takeoverError{409, "client_request_id already identifies a different takeover"}
	}
	current, err := q.GetIssueDelegationReturnForDispatcher(ctx, db.GetIssueDelegationReturnForDispatcherParams{WorkspaceID: in.workspace, IssueID: in.issue, DispatcherAgentID: in.dispatcher})
	return action, current.ID, false, err
}

func validateTakeoverResult(ctx context.Context, q *db.Queries, in takeoverInput) error {
	outcome, err := q.LockAgentTaskForDelegation(ctx, in.result)
	if errors.Is(err, pgx.ErrNoRows) {
		return takeoverError{404, "saved result not found"}
	}
	if err != nil {
		return err
	}
	if outcome.IssueID != in.issue {
		return takeoverError{404, "saved result not found"}
	}
	if outcome.Status != "completed" && !(outcome.Status == "failed" && outcome.DelegationOutcomeFinal) {
		return takeoverError{409, "result is not a final saved outcome"}
	}
	child, err := q.HasRetryTaskForParent(ctx, in.result)
	if err != nil {
		return err
	}
	if child {
		return takeoverError{409, "select the final retry result"}
	}
	_, err = q.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: in.result, DelegationReturnID: in.from})
	if errors.Is(err, pgx.ErrNoRows) {
		return takeoverError{404, "result has no accepted relationship to this dispatcher"}
	}
	return err
}

func (h *Handler) authorizeTakeoverTarget(ctx context.Context, q *db.Queries, in takeoverInput, target db.IssueDelegationReturn, source db.AgentTaskQueue) error {
	actor, err := q.LockAgentForDelegation(ctx, db.LockAgentForDelegationParams{ID: in.dispatcher, WorkspaceID: in.workspace})
	if err != nil {
		return err
	}
	member, err := q.LockResultTakeoverMember(ctx, db.LockResultTakeoverMemberParams{WorkspaceID: in.workspace, UserID: in.actor})
	if errors.Is(err, pgx.ErrNoRows) {
		return takeoverError{403, "workspace membership is unavailable"}
	}
	if err != nil {
		return err
	}
	if !roleAllowed(member.Role, "owner", "admin") && actor.OwnerID != in.actor {
		return takeoverError{403, "only the agent owner can manage this agent"}
	}
	if actor.ArchivedAt.Valid {
		return takeoverError{409, "dispatcher is archived"}
	}
	if _, err = q.LockResultTakeoverMember(ctx, db.LockResultTakeoverMemberParams{WorkspaceID: in.workspace, UserID: actor.OwnerID}); errors.Is(err, pgx.ErrNoRows) {
		return takeoverError{409, "dispatcher owner is unavailable"}
	}
	if err != nil {
		return err
	}
	if in.kind == delegation.KindExternalSession {
		session, err := q.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{ID: target.ExternalSessionID, WorkspaceID: in.workspace})
		if err != nil {
			return err
		}
		if session.OwnerUserID != actor.OwnerID {
			return takeoverError{409, "external session ownership changed"}
		}
		return nil
	}
	runtime, err := q.GetAgentRuntime(ctx, source.RuntimeID)
	if err != nil {
		return err
	}
	if runtime.RuntimeMode != "local" || !service.StrictResultProviderSupported(runtime.Provider) || !service.RuntimeAllowsUser(runtime, actor.OwnerID) {
		return takeoverError{409, "target runtime cannot receive strict result input"}
	}
	if target.ScopeTaskID.Valid {
		return nil
	}
	if source.ChatSessionID.Valid {
		chat, err := q.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: source.ChatSessionID, WorkspaceID: in.workspace})
		if err != nil {
			return err
		}
		if chat.CreatorID != in.actor {
			return takeoverError{403, "not your target chat session"}
		}
	}
	if source.AutopilotRunID.Valid {
		run, err := q.GetAutopilotRun(ctx, source.AutopilotRunID)
		if err != nil {
			return err
		}
		ap, err := q.GetAutopilotInWorkspace(ctx, db.GetAutopilotInWorkspaceParams{ID: run.AutopilotID, WorkspaceID: in.workspace})
		if err != nil {
			return err
		}
		if autopilotWriteByOwnership(ap, member) {
			return nil
		}
		allowed, err := q.IsAutopilotCollaborator(ctx, db.IsAutopilotCollaboratorParams{AutopilotID: ap.ID, UserID: in.actor})
		if err != nil {
			return err
		}
		if !allowed {
			return takeoverError{403, "target autopilot access is unavailable"}
		}
	}
	return nil
}
