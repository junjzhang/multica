package delegation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The caller holds the runtime, agent, current task and current return fences.
// Scope and caller authority have already passed the ordinary reuse checks.
func continueManagedReturn(ctx context.Context, q *db.Queries, prior db.IssueDelegationReturn, original, current db.AgentTaskQueue, runtime db.AgentRuntime) (db.IssueDelegationReturn, error) {
	closed := original.Status == "completed" || original.Status == "failed" || original.Status == "cancelled"
	if !closed || original.ID == current.ID || original.AgentID != current.AgentID ||
		!original.IssueID.Valid || original.IssueID != current.IssueID || prior.ScopeTaskID.Valid ||
		prior.RuntimeID != current.RuntimeID || original.RuntimeID != current.RuntimeID || prior.Provider.String != runtime.Provider {
		return db.IssueDelegationReturn{}, fmt.Errorf("%w: managed continuation requires a terminal source in the same issue and runtime", ErrReturnConflict)
	}
	busy, err := q.HasOtherLiveManagedScopeTask(ctx, db.HasOtherLiveManagedScopeTaskParams{AgentID: current.AgentID, IssueID: current.IssueID, TaskID: current.ID})
	if err != nil {
		return db.IssueDelegationReturn{}, err
	}
	if busy {
		return db.IssueDelegationReturn{}, fmt.Errorf("%w: another run still occupies the managed source scope", ErrReturnConflict)
	}
	next, err := q.CreateDelegationReturnVersion(ctx, db.CreateDelegationReturnVersionParams{
		ID: dbid.NewV7(), WorkspaceID: prior.WorkspaceID, IssueID: prior.IssueID, DispatcherAgentID: prior.DispatcherAgentID,
		ReturnKind: string(KindManagedTask), SourceTaskID: current.ID, RuntimeID: current.RuntimeID,
		Provider: util.StrToText(runtime.Provider), SessionID: current.SessionID, WorkDir: current.WorkDir,
	})
	if err != nil {
		return db.IssueDelegationReturn{}, err
	}
	if err := q.SetManagedReturnPredecessor(ctx, db.SetManagedReturnPredecessorParams{ID: next.ID, PreviousID: prior.ID}); err != nil {
		return db.IssueDelegationReturn{}, err
	}
	if err := q.RetireCurrentDelegationReturn(ctx, db.RetireCurrentDelegationReturnParams{WorkspaceID: prior.WorkspaceID, IssueID: prior.IssueID, DispatcherAgentID: prior.DispatcherAgentID}); err != nil {
		return db.IssueDelegationReturn{}, err
	}
	if err := q.SetCurrentDelegationReturn(ctx, next.ID); err != nil {
		return db.IssueDelegationReturn{}, err
	}
	next.IsCurrent, next.ContinuedFromReturnID = true, prior.ID
	err = RecordContinuationAudit(ctx, q, prior, next, pgtype.UUID{}, "delegation_return_continued")
	return next, err
}

// DeliveryReturnID freezes a forwarded notification's physical address while
// leaving its accepted caller relationship and provenance intact.
func DeliveryReturnID(link db.AgentTaskDelegationReturn) pgtype.UUID {
	if link.DeliveryReturnID.Valid {
		return link.DeliveryReturnID
	}
	return link.DelegationReturnID
}

func RecordContinuationAudit(ctx context.Context, q *db.Queries, prior, next db.IssueDelegationReturn, result pgtype.UUID, action string) error {
	details, err := json.Marshal(map[string]string{
		"from_return_id": util.UUIDToString(prior.ID), "to_return_id": util.UUIDToString(next.ID),
		"from_source_task_id": util.UUIDToString(prior.SourceTaskID), "to_source_task_id": util.UUIDToString(next.SourceTaskID),
		"result_task_id": util.UUIDToString(result), "reason": "managed_terminal_source_continuation",
	})
	if err != nil {
		return err
	}
	_, err = q.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: prior.WorkspaceID, IssueID: prior.IssueID,
		ActorType: util.StrToText("agent"), ActorID: prior.DispatcherAgentID, Action: action, Details: details,
	})
	return err
}
