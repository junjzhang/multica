package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ManagedMailboxResultVisible binds consumption to the frozen conversation and
// business scope, not merely to an agent that also owns other conversations.
func ManagedMailboxResultVisible(ctx context.Context, q *db.Queries, message db.AgentMailboxMessage, caller db.AgentTaskQueue) (bool, error) {
	address, err := q.GetMailboxResultReturn(ctx, db.GetMailboxResultReturnParams{MessageID: message.ID, WorkspaceID: message.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if address.ReturnKind != string(delegation.KindManagedTask) || address.DispatcherAgentID != caller.AgentID ||
		address.RuntimeID != caller.RuntimeID || address.SessionID != caller.SessionID || address.WorkDir != caller.WorkDir {
		return false, nil
	}
	runtime, err := q.GetAgentRuntime(ctx, address.RuntimeID)
	if err != nil {
		return false, err
	}
	if runtime.WorkspaceID != address.WorkspaceID || runtime.Provider != address.Provider.String {
		return false, nil
	}
	anchor, err := q.GetAgentTask(ctx, address.SourceTaskID)
	if err != nil {
		return false, err
	}
	same, err := delegation.SameManagedSourceScope(ctx, q, address, anchor, caller)
	if err != nil || !same {
		return false, err
	}
	incoming, err := q.GetMailboxResultObligation(ctx, message.ID)
	if err != nil {
		return false, err
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: caller.AgentID, WorkspaceID: message.WorkspaceID})
	if err != nil {
		return false, err
	}
	authority, _, err := resultCallerProvenance(ctx, q, incoming, address, anchor, agent)
	if IsDelegationResultRefusal(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return caller.OriginatorUserID == authority.UserID, nil
}

// AckMailbox serializes cancellation with claim/start on the queue row. The
// mailbox remains the sole processing receipt; cancellation is only a wakeup effect.
func (s *TaskService) AckMailbox(ctx context.Context, params db.AckAgentMailboxProcessingParams, callerTaskID pgtype.UUID) (db.AgentMailboxMessage, error) {
	var empty db.AgentMailboxMessage
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	message, err := q.GetAgentMailboxMessage(ctx, db.GetAgentMailboxMessageParams{ID: params.ID, WorkspaceID: params.WorkspaceID})
	if err != nil {
		return empty, err
	}
	if message.RecipientAgentID != params.RecipientAgentID {
		return empty, pgx.ErrNoRows
	}
	locked, err := q.LockMailboxConsumptionTasks(ctx, db.LockMailboxConsumptionTasksParams{MessageID: params.ID, CallerTaskID: callerTaskID})
	if err != nil {
		return empty, err
	}
	var caller, handling db.AgentTaskQueue
	for _, row := range locked {
		if row.AgentTaskQueue.ID == callerTaskID {
			caller = row.AgentTaskQueue
		}
		if row.IsWakeup {
			handling = row.AgentTaskQueue
		}
	}
	if callerTaskID.Valid {
		if !caller.ID.Valid {
			return empty, pgx.ErrNoRows
		}
		_, err = delegation.ResolveManaged(ctx, q, util.UUIDToString(params.WorkspaceID), util.UUIDToString(params.RecipientAgentID), util.UUIDToString(callerTaskID))
		if err != nil {
			return empty, pgx.ErrNoRows
		}
		visible, err := ManagedMailboxResultVisible(ctx, q, message, caller)
		if err != nil {
			return empty, err
		}
		if !visible {
			return empty, pgx.ErrNoRows
		}
	}
	params.HandledByTaskID = callerTaskID
	updated, err := q.AckAgentMailboxProcessing(ctx, params)
	if err != nil {
		return empty, err
	}
	var cancelled db.AgentTaskQueue
	if updated.ProcessingStatus == "handled" && handling.ID.Valid {
		cancelled, err = q.CancelHandledMailboxResultTask(ctx, handling.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
	}
	if cancelled.ID.Valid && callerTaskID.Valid {
		if _, err := q.PropagateResultSourceReturns(ctx, db.PropagateResultSourceReturnsParams{TaskID: callerTaskID}); err != nil {
			return empty, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	if cancelled.ID.Valid {
		s.NotifyTaskFinished(cancelled)
		s.ReconcileAgentStatus(ctx, cancelled.AgentID)
		s.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, cancelled)
	}
	return updated, nil
}
