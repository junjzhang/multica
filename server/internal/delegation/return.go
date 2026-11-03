// Package delegation is the business gate between an explicit agent delegation
// and the durable return address its result will be delivered to.
//
// It resolves the address from evidence the server trusts — a managed run's
// task token, or an authenticated external session registered for the acting
// agent — and records the accepted recipient for one (issue, dispatcher agent)
// pair. It deliberately never picks an agent's "current", "latest" or "only"
// session: a missing or invalid address fails closed before any child work
// exists.
//
// The accepted address is intentionally separate from the dispatch intent.
// A comment or an assignment records which accepted relationships it carries;
// a run links only the intents it actually covers. That is what keeps two
// independent requests to the same worker from inheriting each other's
// recipient, while a genuine coalesce or a rerun preserves every relationship
// it actually covers.
package delegation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Kind is the transport that will carry the result back to the dispatcher.
type Kind string

const (
	// KindExternalSession addresses a registered host session of an external
	// agent. The exact session is frozen on that dispatcher's accepted return.
	KindExternalSession Kind = "external_session"
	// KindManagedTask addresses the managed run that first dispatched the work.
	// The source task identifies provenance. Its physical return address is
	// frozen on acceptance independently of the task's mutable resume pointer.
	KindManagedTask Kind = "managed_task"
)

var (
	// ErrReturnMissing reports that the actor is an agent but this request
	// carries no usable delegation context at all.
	ErrReturnMissing = errors.New("the delegating agent has no return session context")
	// ErrReturnNotReady reports that the managed run exists but its provider
	// session pin has not been persisted yet. It is retryable; the caller must
	// not create child work in the meantime.
	ErrReturnNotReady = errors.New("the delegating run has not recorded its provider session yet")
	// ErrReturnSourceClosed reports that the managed run is no longer active.
	// A completed/failed/cancelled run can remain the target of a relationship
	// it already accepted, but it may not accept a new delegation.
	ErrReturnSourceClosed = errors.New("the delegating run is no longer active")
	// ErrReturnInvalid reports a foreign, cross-workspace, ambiguous, archived,
	// or otherwise unverifiable address.
	ErrReturnInvalid = errors.New("the delegation return address is not valid")
	// ErrReturnConflict reports that this actor already accepted a different
	// physical session for this issue. An ordinary dispatch must not silently
	// keep the old address or overwrite it with the new one; the caller needs
	// an explicit takeover.
	ErrReturnConflict = errors.New("this agent already has a different accepted return session on this issue")
)

// Assignee is the exact assignment slot a dispatch targets. It identifies the
// assignment intent, not the accepted address (which is per dispatcher).
type Assignee struct {
	Type string
	ID   string
}

// Address is one validated return target. ExternalSessionID is the registered
// external_agent_session row; SourceTaskID is the managed agent_task_queue row.
type Address struct {
	DispatcherAgentID string
	Kind              Kind
	ExternalSessionID string
	SourceTaskID      string
	// CreatedBy is the acting member recorded on an external binding. It is
	// empty for managed dispatchers and for callers that only read.
	CreatedBy string
}

// ResolveManaged derives the dispatcher's own managed run from a task id the
// authentication middleware stamped from a validated mat_ token. Every fact is
// re-checked here: a task belonging to another agent or workspace, a run
// without a pinned provider session and work directory, or an already terminal
// run is refused rather than used to accept new child work.
//
// A stored return row may still name a source run that has since completed;
// that is a valid delivery target read by id. It is not evidence that the same
// run may accept another delegation.
func ResolveManaged(ctx context.Context, q *db.Queries, workspaceID, agentID, taskID string) (Address, error) {
	ws, err := util.ParseUUID(strings.TrimSpace(workspaceID))
	if err != nil {
		return Address{}, fmt.Errorf("%w: workspace id", ErrReturnInvalid)
	}
	agent, err := util.ParseUUID(strings.TrimSpace(agentID))
	if err != nil {
		return Address{}, fmt.Errorf("%w: agent id", ErrReturnInvalid)
	}
	taskUUID, err := util.ParseUUID(strings.TrimSpace(taskID))
	if err != nil {
		return Address{}, fmt.Errorf("%w: task id", ErrReturnInvalid)
	}

	task, err := q.GetAgentTask(ctx, taskUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Address{}, fmt.Errorf("%w: source run does not exist", ErrReturnInvalid)
		}
		return Address{}, fmt.Errorf("delegation: load source run: %w", err)
	}
	actor, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agent, WorkspaceID: ws})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Address{}, fmt.Errorf("%w: agent is not in this workspace", ErrReturnInvalid)
		}
		return Address{}, fmt.Errorf("delegation: load agent: %w", err)
	}
	if err := validateManagedSource(ctx, q, ws, agent, task, actor); err != nil {
		return Address{}, err
	}
	return Address{
		DispatcherAgentID: util.UUIDToString(agent),
		Kind:              KindManagedTask,
		SourceTaskID:      util.UUIDToString(task.ID),
	}, nil
}

// validateManagedSource checks every fact that authorizes one managed run to
// accept a delegation. Callers pass either an unlocked snapshot (a fast
// pre-check) or rows they locked in the write transaction (the authoritative
// check).
func validateManagedSource(ctx context.Context, q *db.Queries, ws, agentID pgtype.UUID, task db.AgentTaskQueue, actor db.Agent) error {
	if task.AgentID != agentID {
		return fmt.Errorf("%w: source run belongs to another agent", ErrReturnInvalid)
	}
	if err := requireTaskScope(ctx, q, ws, task); err != nil {
		return err
	}
	if actor.ExecutionKind != "managed" || actor.ArchivedAt.Valid {
		return fmt.Errorf("%w: agent is not an active managed agent", ErrReturnInvalid)
	}
	if !task.RuntimeID.Valid || !actor.RuntimeID.Valid || task.RuntimeID != actor.RuntimeID {
		return fmt.Errorf("%w: source run and agent disagree on the runtime", ErrReturnInvalid)
	}
	switch task.Status {
	case "completed", "failed", "cancelled":
		return fmt.Errorf("%w: source run %s is %s", ErrReturnSourceClosed, util.UUIDToString(task.ID), task.Status)
	}
	if !task.ResourceControlRequired || !task.SessionPinConfirmed || !task.SessionPinGeneration.Valid {
		return fmt.Errorf("%w: source execution has no confirmed resource-control session", ErrReturnNotReady)
	}
	// The provider pin is asynchronous. Until both the session and the work
	// directory are durable there is no address to return to, and the previous
	// run's session is not a substitute.
	if !task.SessionID.Valid || strings.TrimSpace(task.SessionID.String) == "" ||
		!task.WorkDir.Valid || strings.TrimSpace(task.WorkDir.String) == "" {
		return fmt.Errorf("%w: source run %s", ErrReturnNotReady, util.UUIDToString(task.ID))
	}
	return nil
}

// requireTaskScope proves the source run belongs to this workspace through its
// original business scope. The delegated issue is deliberately not a substitute
// scope: a run on a parent issue or chat still returns there.
func requireTaskScope(ctx context.Context, q *db.Queries, ws pgtype.UUID, task db.AgentTaskQueue) error {
	switch {
	case task.IssueID.Valid:
		if _, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: task.IssueID, WorkspaceID: ws}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: source run issue is not in this workspace", ErrReturnInvalid)
			}
			return fmt.Errorf("delegation: load source run issue: %w", err)
		}
		return nil
	case task.ChatSessionID.Valid:
		if _, err := q.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: task.ChatSessionID, WorkspaceID: ws}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: source run chat is not in this workspace", ErrReturnInvalid)
			}
			return fmt.Errorf("delegation: load source run chat: %w", err)
		}
		return nil
	case task.AutopilotRunID.Valid:
		run, err := q.GetAutopilotRun(ctx, task.AutopilotRunID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: source run autopilot is missing", ErrReturnInvalid)
			}
			return fmt.Errorf("delegation: load source run autopilot: %w", err)
		}
		if _, err := q.GetAutopilotInWorkspace(ctx, db.GetAutopilotInWorkspaceParams{ID: run.AutopilotID, WorkspaceID: ws}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: source run autopilot is not in this workspace", ErrReturnInvalid)
			}
			return fmt.Errorf("delegation: load source run autopilot workspace: %w", err)
		}
		return nil
	default:
		// A run with no business scope (a quick create, for example) is still
		// an exact runtime/provider session/work directory. The task token's
		// workspace binding and the agent check above already prove tenancy, so
		// the run itself is the return destination.
		return nil
	}
}

// ResolveExternal turns an explicit current host/session reference into the one
// registered external session of this agent. The reference may be the
// registered session id or the provider's own session id (Paseo's
// PASEO_AGENT_ID); an unregistered, foreign, archived, or ambiguous reference
// is refused. No fallback to the agent's oldest/only session exists.
func ResolveExternal(ctx context.Context, q *db.Queries, workspaceID, agentID, sessionRef string) (Address, error) {
	ref := strings.TrimSpace(sessionRef)
	if ref == "" {
		return Address{}, fmt.Errorf("%w: pass --return-session or set MULTICA_RETURN_SESSION", ErrReturnMissing)
	}
	ws, err := util.ParseUUID(strings.TrimSpace(workspaceID))
	if err != nil {
		return Address{}, fmt.Errorf("%w: workspace id", ErrReturnInvalid)
	}
	agent, err := util.ParseUUID(strings.TrimSpace(agentID))
	if err != nil {
		return Address{}, fmt.Errorf("%w: agent id", ErrReturnInvalid)
	}
	instance, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agent, WorkspaceID: ws})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Address{}, fmt.Errorf("%w: agent is not in this workspace", ErrReturnInvalid)
		}
		return Address{}, fmt.Errorf("delegation: load agent: %w", err)
	}
	if instance.ExecutionKind != "external" || instance.ArchivedAt.Valid {
		return Address{}, fmt.Errorf("%w: agent is not an active external agent", ErrReturnInvalid)
	}

	var matches []db.ExternalAgentSession
	if refUUID, parseErr := util.ParseUUID(ref); parseErr == nil {
		session, lookupErr := q.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{ID: refUUID, WorkspaceID: ws})
		if lookupErr == nil && session.AgentID == agent {
			matches = append(matches, session)
		} else if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
			return Address{}, fmt.Errorf("delegation: load external session: %w", lookupErr)
		}
	}
	if len(matches) == 0 {
		// A provider session id looks like a UUID too, so the registered-id
		// lookup above may legitimately miss. Match the host session identity
		// exactly within this agent and workspace; more than one row is
		// ambiguous and refused.
		rows, listErr := q.ListExternalAgentSessionsByHostSession(ctx, db.ListExternalAgentSessionsByHostSessionParams{
			WorkspaceID:   ws,
			AgentID:       agent,
			HostSessionID: ref,
		})
		if listErr != nil {
			return Address{}, fmt.Errorf("delegation: match external session: %w", listErr)
		}
		matches = rows
	}
	switch len(matches) {
	case 0:
		return Address{}, fmt.Errorf("%w: no session %q is registered to this agent in this workspace", ErrReturnInvalid, ref)
	case 1:
		return Address{
			DispatcherAgentID: util.UUIDToString(agent),
			Kind:              KindExternalSession,
			ExternalSessionID: util.UUIDToString(matches[0].ID),
		}, nil
	default:
		return Address{}, fmt.Errorf("%w: session reference %q is ambiguous (%d registered sessions)", ErrReturnInvalid, ref, len(matches))
	}
}

// Ensure records addr as the accepted return for one (issue, dispatcher agent)
// pair and returns the row that was accepted. The first accepted address for
// that key wins: an existing row is returned unchanged when the current
// trusted source is the same physical session. A different physical session for
// the same actor on the same issue is a conflict, not a silent reuse of the old
// run and not a silent overwrite with the new one.
//
// A managed address is re-validated under a row lock inside the caller's
// transaction. Passing a transaction-bound *db.Queries is therefore required
// when the row must commit with the delegated issue, comment, or run; an
// already completed source run is refused here even when the pre-write read saw
// it active, while a previously accepted row that names a completed source
// remains a valid delivery target.
func Ensure(ctx context.Context, q *db.Queries, workspaceID, issueID string, addr Address, createdBy string) (db.IssueDelegationReturn, error) {
	ws, err := util.ParseUUID(strings.TrimSpace(workspaceID))
	if err != nil {
		return db.IssueDelegationReturn{}, fmt.Errorf("delegation: workspace id: %w", err)
	}
	issue, err := util.ParseUUID(strings.TrimSpace(issueID))
	if err != nil {
		return db.IssueDelegationReturn{}, fmt.Errorf("delegation: issue id: %w", err)
	}
	dispatcher, err := util.ParseUUID(strings.TrimSpace(addr.DispatcherAgentID))
	if err != nil {
		return db.IssueDelegationReturn{}, fmt.Errorf("delegation: dispatcher agent id: %w", err)
	}

	var (
		currentTask    db.AgentTaskQueue
		currentRuntime db.AgentRuntime
		binding        sessionroute.Binding
		hasBinding     bool
		managedInput   bool
	)
	switch addr.Kind {
	case KindManagedTask:
		if strings.TrimSpace(addr.SourceTaskID) == "" {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: source task id", ErrReturnInvalid)
		}
		sourceTaskID, parseErr := util.ParseUUID(strings.TrimSpace(addr.SourceTaskID))
		if parseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: source task id", ErrReturnInvalid)
		}
		task, runtime, checkErr := recheckManagedSourceLocked(ctx, q, ws, dispatcher, sourceTaskID)
		if checkErr != nil {
			return db.IssueDelegationReturn{}, checkErr
		}
		currentTask = task
		currentRuntime = runtime
		managedInput = true
	case KindExternalSession:
		if strings.TrimSpace(addr.ExternalSessionID) == "" {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: external session id", ErrReturnInvalid)
		}
		sessionID, parseErr := util.ParseUUID(strings.TrimSpace(addr.ExternalSessionID))
		if parseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: external session id", ErrReturnInvalid)
		}
		// Reuse the reviewed binding. The first accepted target for
		// (agent, issue) is fixed, so an ordinary dispatch from a different
		// physical session cannot retarget it; only the explicit takeover
		// endpoint calls Bind.
		binding, err = sessionroute.Ensure(ctx, q, sessionroute.BindRequest{
			WorkspaceID: util.UUIDToString(ws),
			AgentID:     util.UUIDToString(dispatcher),
			IssueID:     util.UUIDToString(issue),
			SessionID:   util.UUIDToString(sessionID),
			CreatedBy:   firstNonEmpty(createdBy, addr.CreatedBy),
		})
		if err != nil {
			if errors.Is(err, sessionroute.ErrInvalidBinding) {
				return db.IssueDelegationReturn{}, fmt.Errorf("%w: %v", ErrReturnInvalid, err)
			}
			return db.IssueDelegationReturn{}, err
		}
		if binding.SessionID != util.UUIDToString(sessionID) {
			// The accepted binding is a different physical session and this
			// request is not an explicit takeover. Do not silently deliver to
			// either one.
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: current session %s, accepted binding %s", ErrReturnConflict, util.UUIDToString(sessionID), binding.SessionID)
		}
		hasBinding = true
	default:
		return db.IssueDelegationReturn{}, fmt.Errorf("%w: unknown return kind %q", ErrReturnInvalid, addr.Kind)
	}

	lookup := db.LockIssueDelegationReturnForDispatcherParams{
		WorkspaceID:       ws,
		IssueID:           issue,
		DispatcherAgentID: dispatcher,
	}
	existing, err := q.LockIssueDelegationReturnForDispatcher(ctx, lookup)
	if err == nil {
		return reuseAccepted(ctx, q, existing, currentTask, currentRuntime, managedInput, binding, hasBinding)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.IssueDelegationReturn{}, fmt.Errorf("delegation: load accepted return: %w", err)
	}

	params := db.CreateIssueDelegationReturnParams{
		ID:                dbid.NewV7(),
		WorkspaceID:       ws,
		IssueID:           issue,
		DispatcherAgentID: dispatcher,
		ReturnKind:        string(addr.Kind),
	}
	switch addr.Kind {
	case KindExternalSession:
		sessionID, parseErr := util.ParseUUID(strings.TrimSpace(binding.SessionID))
		if parseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: accepted binding session", ErrReturnInvalid)
		}
		params.ExternalSessionID = sessionID
	case KindManagedTask:
		params.SourceTaskID = currentTask.ID
		params.RuntimeID = currentTask.RuntimeID
		params.Provider = pgtype.Text{String: currentRuntime.Provider, Valid: true}
		params.SessionID = currentTask.SessionID
		params.WorkDir = currentTask.WorkDir
		scopeTaskID, err := ManagedSourceTaskScope(ctx, q, currentTask, ws)
		if err != nil {
			return db.IssueDelegationReturn{}, err
		}
		params.ScopeTaskID = scopeTaskID
	}

	created, err := q.CreateIssueDelegationReturn(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent dispatch for the same key committed first. Its
			// accepted recipient is the answer; never replace it.
			winner, lockErr := q.LockIssueDelegationReturnForDispatcher(ctx, lookup)
			if lockErr != nil {
				return db.IssueDelegationReturn{}, fmt.Errorf("delegation: load winning accepted return: %w", lockErr)
			}
			return reuseAccepted(ctx, q, winner, currentTask, currentRuntime, managedInput, binding, hasBinding)
		}
		return db.IssueDelegationReturn{}, fmt.Errorf("delegation: record accepted return: %w", err)
	}
	return created, nil
}

// reuseAccepted applies the fixed-address rule to an existing row. The same
// physical session is reusable even when a continuation turn carries a new task
// id; a different session is a conflict.
func reuseAccepted(ctx context.Context, q *db.Queries, existing db.IssueDelegationReturn, currentTask db.AgentTaskQueue, currentRuntime db.AgentRuntime, managedInput bool, binding sessionroute.Binding, hasBinding bool) (db.IssueDelegationReturn, error) {
	switch {
	case existing.ReturnKind == string(KindManagedTask) && managedInput:
		if !existing.RuntimeID.Valid || !nonemptyText(existing.Provider) ||
			!nonemptyText(existing.SessionID) || !nonemptyText(existing.WorkDir) {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: accepted managed address snapshot is missing", ErrReturnConflict)
		}
		original, err := q.GetAgentTask(ctx, existing.SourceTaskID)
		if err != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: original source scope unavailable", ErrReturnConflict)
		}
		if !original.IssueID.Valid && !original.ChatSessionID.Valid && !original.AutopilotRunID.Valid {
			if err := ValidateOriginalTaskScope(ctx, q, existing); err != nil {
				return db.IssueDelegationReturn{}, fmt.Errorf("%w: %v", ErrReturnConflict, err)
			}
		}
		if existing.RuntimeID == currentTask.RuntimeID &&
			existing.Provider.String == currentRuntime.Provider &&
			existing.SessionID.String == currentTask.SessionID.String &&
			existing.WorkDir.String == currentTask.WorkDir.String {
			return existing, nil
		}
		return db.IssueDelegationReturn{}, fmt.Errorf("%w: current managed address differs from the accepted snapshot", ErrReturnConflict)
	case existing.ReturnKind == string(KindExternalSession) && hasBinding:
		if uuidEqual(existing.ExternalSessionID, parseUUIDOrZero(binding.SessionID)) {
			return existing, nil
		}
		// The explicit takeover endpoint already moved the binding to the
		// session this request authenticated from. Follow that explicit change;
		// an ordinary dispatch from a different session never reaches here
		// because it would not match the binding above.
		sessionID, parseErr := util.ParseUUID(binding.SessionID)
		if parseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: accepted binding session", ErrReturnInvalid)
		}
		updated, updateErr := q.UpdateIssueDelegationReturnExternalSession(ctx, db.UpdateIssueDelegationReturnExternalSessionParams{
			ID:                existing.ID,
			ExternalSessionID: sessionID,
		})
		if updateErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("delegation: follow explicit takeover: %w", updateErr)
		}
		return updated, nil
	default:
		return db.IssueDelegationReturn{}, fmt.Errorf("%w: accepted return kind %s does not match this dispatcher", ErrReturnConflict, existing.ReturnKind)
	}
}

func nonemptyText(value pgtype.Text) bool {
	return value.Valid && strings.TrimSpace(value.String) != ""
}

// Admission locks runtime, agent, then source task, matching claim delivery.
// The initial task read only locates the runtime; every authority and address
// value is checked again under those locks before the snapshot can be accepted.
func recheckManagedSourceLocked(ctx context.Context, q *db.Queries, ws, dispatcher pgtype.UUID, sourceTaskID pgtype.UUID) (db.AgentTaskQueue, db.AgentRuntime, error) {
	candidate, err := q.GetAgentTask(ctx, sourceTaskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: source run does not exist", ErrReturnInvalid)
		}
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("delegation: locate source runtime: %w", err)
	}
	if candidate.AgentID != dispatcher || !candidate.RuntimeID.Valid {
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: source run identity", ErrReturnInvalid)
	}
	runtime, err := q.LockRuntimeForDelegation(ctx, db.LockRuntimeForDelegationParams{ID: candidate.RuntimeID, WorkspaceID: ws})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: source runtime does not exist in this workspace", ErrReturnInvalid)
		}
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("delegation: lock source runtime: %w", err)
	}
	if strings.TrimSpace(runtime.Provider) == "" {
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: source runtime provider is missing", ErrReturnInvalid)
	}
	actor, err := q.LockAgentForDelegation(ctx, db.LockAgentForDelegationParams{ID: dispatcher, WorkspaceID: ws})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: agent is not in this workspace", ErrReturnInvalid)
		}
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("delegation: lock agent: %w", err)
	}
	task, err := q.LockAgentTaskForDelegation(ctx, sourceTaskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: source run does not exist", ErrReturnInvalid)
		}
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("delegation: lock source run: %w", err)
	}
	if task.RuntimeID != runtime.ID {
		return db.AgentTaskQueue{}, db.AgentRuntime{}, fmt.Errorf("%w: source runtime changed before acceptance", ErrReturnInvalid)
	}
	if err := validateManagedSource(ctx, q, ws, dispatcher, task, actor); err != nil {
		return db.AgentTaskQueue{}, db.AgentRuntime{}, err
	}
	return task, runtime, nil
}

// RunReturnIDs returns every accepted relationship one run carries.
func RunReturnIDs(ctx context.Context, q *db.Queries, taskID pgtype.UUID) ([]pgtype.UUID, error) {
	if !taskID.Valid {
		return nil, nil
	}
	rows, err := q.ListAgentTaskDelegationReturns(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("delegation: list run returns: %w", err)
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// CommentReturnIDs returns the accepted relationships one comment intent
// carries. It is the only source new, folded, replayed, and retriggered runs
// use for a comment.
func CommentReturnIDs(ctx context.Context, q *db.Queries, commentID pgtype.UUID) ([]pgtype.UUID, error) {
	if !commentID.Valid {
		return nil, nil
	}
	ids, err := q.ListCommentDelegationReturnIDs(ctx, commentID)
	if err != nil {
		return nil, fmt.Errorf("delegation: list comment returns: %w", err)
	}
	return ids, nil
}

// AssignmentIntent is one assignment slot's persisted dispatch intent.
type AssignmentIntent struct {
	Assignee  Assignee
	TaskID    pgtype.UUID
	ReturnIDs []pgtype.UUID
}

// LoadAssignmentIntent returns the persisted intent for one assignment slot,
// including the exact accepted relationships it carries. It returns nil when
// the slot has no intent.
func LoadAssignmentIntent(ctx context.Context, q *db.Queries, issueID pgtype.UUID, assignee Assignee) (*AssignmentIntent, error) {
	if !issueID.Valid {
		return nil, nil
	}
	assigneeID, err := util.ParseUUID(strings.TrimSpace(assignee.ID))
	if err != nil {
		return nil, fmt.Errorf("%w: assignee id", ErrReturnInvalid)
	}
	header, err := q.GetIssueAssignmentDelegation(ctx, db.GetIssueAssignmentDelegationParams{
		IssueID:      issueID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("delegation: load assignment intent: %w", err)
	}
	ids, err := q.ListIssueAssignmentDelegationReturnIDs(ctx, db.ListIssueAssignmentDelegationReturnIDsParams{
		IssueID:      issueID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
	})
	if err != nil {
		return nil, fmt.Errorf("delegation: list assignment intent returns: %w", err)
	}
	return &AssignmentIntent{Assignee: assignee, TaskID: header.TaskID, ReturnIDs: ids}, nil
}

// StoreAssignmentIntent replaces one slot's current intent with the exact set a
// dispatch accepts. The header and set commit with the issue write.
func StoreAssignmentIntent(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID, assignee Assignee, returnIDs []pgtype.UUID, taskID pgtype.UUID) error {
	assigneeID, err := util.ParseUUID(strings.TrimSpace(assignee.ID))
	if err != nil {
		return fmt.Errorf("%w: assignee id", ErrReturnInvalid)
	}
	params := db.UpsertIssueAssignmentDelegationParams{
		IssueID:      issueID,
		WorkspaceID:  workspaceID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
	}
	if taskID.Valid {
		params.TaskID = taskID
	}
	if _, err := q.UpsertIssueAssignmentDelegation(ctx, params); err != nil {
		return fmt.Errorf("delegation: store assignment intent: %w", err)
	}
	if err := q.DeleteIssueAssignmentDelegationReturns(ctx, db.DeleteIssueAssignmentDelegationReturnsParams{
		IssueID:      issueID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
	}); err != nil {
		return fmt.Errorf("delegation: clear assignment intent returns: %w", err)
	}
	for _, returnID := range UnionReturnIDs(returnIDs) {
		if _, err := q.LinkIssueAssignmentDelegationReturn(ctx, db.LinkIssueAssignmentDelegationReturnParams{
			IssueID:            issueID,
			AssigneeType:       assignee.Type,
			AssigneeID:         assigneeID,
			DelegationReturnID: returnID,
		}); err != nil {
			return fmt.Errorf("delegation: link assignment intent return: %w", err)
		}
	}
	return nil
}

// SetAssignmentIntentTask records the run that finally carries a backlog intent.
func SetAssignmentIntentTask(ctx context.Context, q *db.Queries, issueID pgtype.UUID, assignee Assignee, taskID pgtype.UUID) error {
	assigneeID, err := util.ParseUUID(strings.TrimSpace(assignee.ID))
	if err != nil {
		return fmt.Errorf("%w: assignee id", ErrReturnInvalid)
	}
	return q.SetIssueAssignmentDelegationTask(ctx, db.SetIssueAssignmentDelegationTaskParams{
		IssueID:      issueID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
		TaskID:       taskID,
	})
}

// DeleteAssignmentIntent removes a replaced assignment's intent. Its accepted
// relationships live on in the run that carried them; keeping the header would
// resurrect a stale dispatcher if the slot were assigned again much later.
func DeleteAssignmentIntent(ctx context.Context, q *db.Queries, issueID pgtype.UUID, assignee Assignee) error {
	assigneeID, err := util.ParseUUID(strings.TrimSpace(assignee.ID))
	if err != nil {
		return fmt.Errorf("%w: assignee id", ErrReturnInvalid)
	}
	if err := q.DeleteIssueAssignmentDelegationReturns(ctx, db.DeleteIssueAssignmentDelegationReturnsParams{
		IssueID:      issueID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
	}); err != nil {
		return fmt.Errorf("delegation: clear assignment intent returns: %w", err)
	}
	return q.DeleteIssueAssignmentDelegation(ctx, db.DeleteIssueAssignmentDelegationParams{
		IssueID:      issueID,
		AssigneeType: assignee.Type,
		AssigneeID:   assigneeID,
	})
}

// LinkRun attaches the accepted relationships a run was dispatched under. The
// insert is idempotent, so coalescing, reruns, and retries can add a
// relationship without dropping the run's existing set. Callers pass the same
// transaction that created or folded the run.
func LinkRun(ctx context.Context, q *db.Queries, taskID pgtype.UUID, returnIDs ...pgtype.UUID) error {
	if !taskID.Valid {
		return nil
	}
	for _, returnID := range UnionReturnIDs(returnIDs) {
		if _, err := q.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{
			TaskID:             taskID,
			DelegationReturnID: returnID,
		}); err != nil {
			return fmt.Errorf("delegation: link run return: %w", err)
		}
	}
	return nil
}

// LinkCommentReturn records the accepted relation between one comment intent
// and one dispatcher address. It is idempotent.
func LinkCommentReturn(ctx context.Context, q *db.Queries, commentID, returnID pgtype.UUID) error {
	if !commentID.Valid || !returnID.Valid {
		return nil
	}
	if _, err := q.LinkCommentDelegationReturn(ctx, db.LinkCommentDelegationReturnParams{
		CommentID:          commentID,
		DelegationReturnID: returnID,
	}); err != nil {
		return fmt.Errorf("delegation: link comment return: %w", err)
	}
	return nil
}

// UnionReturnIDs merges return sets without duplicates, preserving first-seen
// order.
func UnionReturnIDs(sets ...[]pgtype.UUID) []pgtype.UUID {
	seen := make(map[pgtype.UUID]struct{})
	var out []pgtype.UUID
	for _, set := range sets {
		for _, id := range set {
			if !id.Valid {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func uuidEqual(a, b pgtype.UUID) bool {
	if !a.Valid || !b.Valid {
		return false
	}
	return a == b
}

func parseUUIDOrZero(value string) pgtype.UUID {
	id, err := util.ParseUUID(strings.TrimSpace(value))
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
