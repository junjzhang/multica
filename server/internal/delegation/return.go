// Package delegation is the business gate between an explicit agent delegation
// and the durable return address its result will be delivered to.
//
// It resolves the address from evidence the server trusts — a managed run's
// task token, or an authenticated external session registered for the acting
// agent — and records the accepted recipient for one (issue, dispatcher agent)
// relationship. It deliberately never picks an agent's "current", "latest" or
// "only" session: a missing or invalid address fails closed before any child
// work exists.
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
	// KindManagedTask addresses the managed run that dispatched the work. The
	// source task row owns the runtime, provider session and work directory;
	// the next slice resumes that row instead of the agent+issue latest session.
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
	// ErrReturnInvalid reports a foreign, cross-workspace, ambiguous, archived,
	// or otherwise unverifiable address.
	ErrReturnInvalid = errors.New("the delegation return address is not valid")
)

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
// re-checked here: a task belonging to another agent or workspace, or a run
// without a pinned provider session and work directory, is refused rather than
// used as an address. A completed source run is still a valid destination: the
// result-handling turn resumes its recorded provider session, and the task
// token's own lifecycle is what retires the credential.
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
	if task.AgentID != agent {
		return Address{}, fmt.Errorf("%w: source run belongs to another agent", ErrReturnInvalid)
	}
	if err := requireTaskScope(ctx, q, ws, task); err != nil {
		return Address{}, err
	}

	actor, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agent, WorkspaceID: ws})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Address{}, fmt.Errorf("%w: agent is not in this workspace", ErrReturnInvalid)
		}
		return Address{}, fmt.Errorf("delegation: load agent: %w", err)
	}
	if actor.ExecutionKind != "managed" || actor.ArchivedAt.Valid {
		return Address{}, fmt.Errorf("%w: agent is not an active managed agent", ErrReturnInvalid)
	}
	if !task.RuntimeID.Valid || !actor.RuntimeID.Valid || task.RuntimeID != actor.RuntimeID {
		return Address{}, fmt.Errorf("%w: source run and agent disagree on the runtime", ErrReturnInvalid)
	}
	// The provider pin is asynchronous. Until both the session and the work
	// directory are durable there is no address to return to, and the previous
	// run's session is not a substitute.
	if !task.SessionID.Valid || strings.TrimSpace(task.SessionID.String) == "" ||
		!task.WorkDir.Valid || strings.TrimSpace(task.WorkDir.String) == "" {
		return Address{}, fmt.Errorf("%w: source run %s", ErrReturnNotReady, util.UUIDToString(task.ID))
	}
	return Address{
		DispatcherAgentID: util.UUIDToString(agent),
		Kind:              KindManagedTask,
		SourceTaskID:      util.UUIDToString(task.ID),
	}, nil
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
// key and returns the row that was accepted. The first accepted delegation for
// that key wins: an existing row is returned unchanged, so a later assignment,
// rerun, mention, or folded comment by the same actor cannot silently retarget
// its own result. A different dispatcher has a different key and therefore its
// own accepted relationship. Pass a transaction-bound *db.Queries when the row
// must commit with the delegated issue or comment.
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
	lookup := db.GetIssueDelegationReturnForDispatcherParams{
		WorkspaceID:       ws,
		IssueID:           issue,
		DispatcherAgentID: dispatcher,
	}

	if existing, err := q.GetIssueDelegationReturnForDispatcher(ctx, lookup); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
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
		sessionID, parseErr := util.ParseUUID(strings.TrimSpace(addr.ExternalSessionID))
		if parseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: external session id", ErrReturnInvalid)
		}
		// Reuse the reviewed binding: producers resolve the same
		// (agent, issue) target, and an existing accepted binding is not
		// overwritten by an ordinary dispatch. An explicit takeover is the
		// only writer allowed to replace it, so the accepted return follows
		// the binding's frozen target rather than the caller's current
		// session.
		binding, bindErr := sessionroute.Ensure(ctx, q, sessionroute.BindRequest{
			WorkspaceID: util.UUIDToString(ws),
			AgentID:     util.UUIDToString(dispatcher),
			IssueID:     util.UUIDToString(issue),
			SessionID:   util.UUIDToString(sessionID),
			CreatedBy:   firstNonEmpty(createdBy, addr.CreatedBy),
		})
		if bindErr != nil {
			if errors.Is(bindErr, sessionroute.ErrInvalidBinding) {
				return db.IssueDelegationReturn{}, fmt.Errorf("%w: %v", ErrReturnInvalid, bindErr)
			}
			return db.IssueDelegationReturn{}, bindErr
		}
		bindingSession, bindParseErr := util.ParseUUID(binding.SessionID)
		if bindParseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: accepted binding session", ErrReturnInvalid)
		}
		params.ExternalSessionID = bindingSession
	case KindManagedTask:
		sourceTaskID, parseErr := util.ParseUUID(strings.TrimSpace(addr.SourceTaskID))
		if parseErr != nil {
			return db.IssueDelegationReturn{}, fmt.Errorf("%w: source task id", ErrReturnInvalid)
		}
		params.SourceTaskID = sourceTaskID
	default:
		return db.IssueDelegationReturn{}, fmt.Errorf("%w: unknown return kind %q", ErrReturnInvalid, addr.Kind)
	}

	created, err := q.CreateIssueDelegationReturn(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent dispatch by the same actor committed first. Its
			// accepted recipient is the answer; never replace it.
			return q.GetIssueDelegationReturnForDispatcher(ctx, lookup)
		}
		return db.IssueDelegationReturn{}, fmt.Errorf("delegation: record accepted return: %w", err)
	}
	return created, nil
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

// IssueReturnIDs returns every accepted relationship recorded on an issue,
// oldest first. An executor reassignment preserves the complete set so the
// original recipients still receive the result.
func IssueReturnIDs(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID) ([]pgtype.UUID, error) {
	rows, err := q.ListIssueDelegationReturns(ctx, db.ListIssueDelegationReturnsParams{
		WorkspaceID: workspaceID,
		IssueID:     issueID,
	})
	if err != nil {
		return nil, fmt.Errorf("delegation: list issue returns: %w", err)
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// UnlinkedReturnIDs returns the accepted relationships recorded on an issue
// that no run carries yet. A run starting on that issue must attach them so a
// backlog delegation is not guessed away.
func UnlinkedReturnIDs(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID) ([]pgtype.UUID, error) {
	rows, err := q.ListUnlinkedIssueDelegationReturns(ctx, db.ListUnlinkedIssueDelegationReturnsParams{
		WorkspaceID: workspaceID,
		IssueID:     issueID,
	})
	if err != nil {
		return nil, fmt.Errorf("delegation: list unlinked returns: %w", err)
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// LatestRunReturnIDs returns the accepted relationships of the most recent
// earlier run for one issue/executor pair, excluding excludeTaskID. A rerun or
// executor reassignment copies that run's set instead of picking an arbitrary
// issue-level row.
func LatestRunReturnIDs(ctx context.Context, q *db.Queries, issueID, agentID, excludeTaskID pgtype.UUID) ([]pgtype.UUID, error) {
	if !issueID.Valid || !agentID.Valid {
		return nil, nil
	}
	latest, err := q.GetLatestTaskIDForIssueAndAgent(ctx, db.GetLatestTaskIDForIssueAndAgentParams{
		IssueID:       issueID,
		AgentID:       agentID,
		ExcludeTaskID: excludeTaskID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("delegation: load latest run: %w", err)
	}
	return RunReturnIDs(ctx, q, latest)
}

// LinkRun attaches the accepted relationships a run was dispatched under. The
// insert is idempotent, so coalescing, reruns, and retries can add a
// relationship without dropping the run's existing set.
func LinkRun(ctx context.Context, q *db.Queries, taskID pgtype.UUID, returnIDs ...pgtype.UUID) error {
	if !taskID.Valid {
		return nil
	}
	seen := make(map[pgtype.UUID]struct{}, len(returnIDs))
	for _, returnID := range returnIDs {
		if !returnID.Valid {
			continue
		}
		if _, duplicate := seen[returnID]; duplicate {
			continue
		}
		seen[returnID] = struct{}{}
		if _, err := q.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{
			TaskID:             taskID,
			DelegationReturnID: returnID,
		}); err != nil {
			return fmt.Errorf("delegation: link run return: %w", err)
		}
	}
	return nil
}

// LinkComment attaches one comment's accepted relationship to every run that
// comment became part of, including a pending run it was folded into. It runs
// after the comment and its trigger side committed; the relationship itself is
// already durable with the comment.
func LinkComment(ctx context.Context, q *db.Queries, issueID, commentID, returnID pgtype.UUID) error {
	if !issueID.Valid || !commentID.Valid || !returnID.Valid {
		return nil
	}
	if _, err := q.LinkCommentTasksToDelegationReturn(ctx, db.LinkCommentTasksToDelegationReturnParams{
		DelegationReturnID: returnID,
		IssueID:            issueID,
		CommentID:          commentID,
	}); err != nil {
		return fmt.Errorf("delegation: link comment tasks: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
