package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var ErrManagedWaitConflict = errors.New("managed wait conflicts with its task or accepted request")

func TaskBackgroundWait(task db.AgentTaskQueue) (*protocol.ManagedWait, error) {
	var context struct {
		Wait *protocol.ManagedWait `json:"background_wait"`
	}
	if len(task.Context) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(task.Context, &context); err != nil {
		return nil, err
	}
	if context.Wait == nil || context.Wait.Address.TaskID != util.UUIDToString(task.ID) {
		return nil, nil
	}
	return context.Wait, nil
}

func storeTaskWait(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, wait *protocol.ManagedWait) error {
	data, err := json.Marshal(wait)
	if err != nil {
		return err
	}
	_, err = q.SetManagedWaitContext(ctx, db.SetManagedWaitContextParams{TaskID: task.ID, Wait: data})
	return err
}

// RegisterManagedWait records only a fixed return obligation. It never starts
// execution: the daemon must separately authorize cwd and its active mat_ token.
func (s *TaskService) RegisterManagedWait(ctx context.Context, workspace, actor, taskID pgtype.UUID, req protocol.WaitAuthorization) (protocol.WaitAddress, error) {
	var out protocol.WaitAddress
	err := s.runInTx(ctx, func(q *db.Queries) error {
		task, err := q.LockTaskSessionPin(ctx, taskID)
		if err != nil {
			return err
		}
		if _, err := delegation.ResolveManaged(ctx, q, util.UUIDToString(workspace), util.UUIDToString(actor), util.UUIDToString(taskID)); err != nil {
			return err
		}
		runtime, err := q.GetAgentRuntime(ctx, task.RuntimeID)
		if err != nil {
			return err
		}
		if task.Status != "running" || runtime.DaemonID.String != req.DaemonID || runtime.WorkspaceID != workspace || !StrictResultProviderSupported(runtime.Provider) {
			return ErrManagedWaitConflict
		}
		old, err := TaskBackgroundWait(task)
		if err != nil {
			return err
		}
		if old != nil && old.ID == req.ID {
			if old.RequestHash != req.RequestHash || !old.Deadline.Equal(req.Deadline) {
				return ErrManagedWaitConflict
			}
			out = old.Address
			return nil
		}
		if old != nil && !old.Consumed {
			return ErrManagedWaitConflict
		}
		if req.Deadline.IsZero() || !req.Deadline.After(time.Now()) || req.Deadline.After(time.Now().Add(7*24*time.Hour)) {
			return ErrManagedWaitConflict
		}
		out = protocol.WaitAddress{WorkspaceID: util.UUIDToString(workspace), AgentID: util.UUIDToString(actor), TaskID: util.UUIDToString(task.ID), RuntimeID: util.UUIDToString(runtime.ID), DaemonID: req.DaemonID, Provider: runtime.Provider, SessionID: task.SessionID.String, WorkDir: task.WorkDir.String}
		return storeTaskWait(ctx, q, task, &protocol.ManagedWait{ID: req.ID, Generation: util.UUIDToString(task.SessionPinGeneration), Address: out, RequestHash: req.RequestHash, Deadline: req.Deadline.UTC()})
	})
	return out, err
}

func managedWaitKey(id string) string { return "background_wait:" + id }

func (s *TaskService) CompleteManagedWait(ctx context.Context, result protocol.WaitResult) (string, error) {
	var messageID string
	taskID, err := util.ParseUUID(result.Address.TaskID)
	if err != nil {
		return "", err
	}
	err = s.runInTx(ctx, func(q *db.Queries) error {
		task, err := q.LockTaskSessionPin(ctx, taskID)
		if err != nil {
			return err
		}
		messageID, err = completeManagedWait(ctx, q, task, result)
		return err
	})
	return messageID, err
}

func completeManagedWait(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, result protocol.WaitResult) (string, error) {
	workspace, err := util.ParseUUID(result.Address.WorkspaceID)
	if err != nil {
		return "", err
	}
	if err := delegation.ValidateTaskScope(ctx, q, workspace, task); err != nil {
		return "", err
	}
	if util.UUIDToString(task.AgentID) != result.Address.AgentID || util.UUIDToString(task.RuntimeID) != result.Address.RuntimeID {
		return "", ErrManagedWaitConflict
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	wait, err := TaskBackgroundWait(task)
	if err != nil {
		return "", err
	}
	// Completed older registrations remain in the mailbox after the task has
	// consumed them and registered another wait. Replays cannot mutate the new one.
	if wait == nil || wait.ID != result.ID {
		existing, err := q.GetDeliveryByNotificationKey(ctx, db.GetDeliveryByNotificationKeyParams{WorkspaceID: workspace, NotificationKey: pgtype.Text{String: managedWaitKey(result.ID), Valid: true}})
		if err != nil || !jsonEqual(existing.Payload, data) {
			return "", ErrManagedWaitConflict
		}
		return util.UUIDToString(existing.ID), nil
	}
	if wait.Address != result.Address {
		return "", ErrManagedWaitConflict
	}
	message, err := q.CreateManagedWaitMailbox(ctx, db.CreateManagedWaitMailboxParams{ID: dbid.NewV7(), WorkspaceID: workspace, IssueID: task.IssueID, AgentID: task.AgentID, NotificationKey: pgtype.Text{String: managedWaitKey(result.ID), Valid: true}, Payload: data})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrManagedWaitConflict
	}
	if err != nil {
		return "", err
	}
	wait.Result, wait.DeliveryID = &result, util.UUIDToString(message.ID)
	if err := storeTaskWait(ctx, q, task, wait); err != nil {
		return "", err
	}
	if wait.Suspended && !wait.Consumed && !terminalResourceTask(task.Status) {
		if err := q.WakeTaskForBackgroundWait(ctx, task.ID); err != nil {
			return "", err
		}
	}
	return wait.DeliveryID, nil
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	left, _ := json.Marshal(x)
	right, _ := json.Marshal(y)
	return string(left) == string(right)
}

func consumeTaskWait(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, wait *protocol.ManagedWait) error {
	if wait.Result == nil || wait.DeliveryID == "" {
		return ErrManagedWaitConflict
	}
	messageID, err := util.ParseUUID(wait.DeliveryID)
	if err != nil {
		return err
	}
	workspace, err := util.ParseUUID(wait.Address.WorkspaceID)
	if err != nil {
		return err
	}
	if _, err := q.AckAgentMailboxProcessing(ctx, db.AckAgentMailboxProcessingParams{ID: messageID, WorkspaceID: workspace, RecipientAgentID: task.AgentID, State: "handled", HandledByTaskID: task.ID}); err != nil {
		return err
	}
	wait.Consumed = true
	return storeTaskWait(ctx, q, task, wait)
}

func (s *TaskService) ReadManagedWait(ctx context.Context, workspace, actor, taskID pgtype.UUID, id string, consume bool) (*protocol.ManagedWait, error) {
	var wait *protocol.ManagedWait
	err := s.runInTx(ctx, func(q *db.Queries) error {
		task, err := q.LockTaskSessionPin(ctx, taskID)
		if err != nil {
			return err
		}
		if task.AgentID != actor || task.Status != "running" {
			return ErrManagedWaitConflict
		}
		if err := delegation.ValidateTaskScope(ctx, q, workspace, task); err != nil {
			return err
		}
		wait, err = TaskBackgroundWait(task)
		if err != nil {
			return err
		}
		if wait == nil || wait.ID != id {
			message, err := q.GetDeliveryByNotificationKey(ctx, db.GetDeliveryByNotificationKeyParams{WorkspaceID: workspace, NotificationKey: pgtype.Text{String: managedWaitKey(id), Valid: true}})
			if err != nil {
				return err
			}
			var result protocol.WaitResult
			if json.Unmarshal(message.Payload, &result) != nil || result.Address.TaskID != util.UUIDToString(task.ID) || message.RecipientAgentID != actor || message.ProcessingStatus != "handled" || message.TargetSessionID.Valid {
				return pgx.ErrNoRows
			}
			wait = &protocol.ManagedWait{ID: id, Address: result.Address, Consumed: true, Result: &result, DeliveryID: util.UUIDToString(message.ID)}
			return nil
		}
		if wait.Result != nil && consume {
			return consumeTaskWait(ctx, q, task, wait)
		}
		return nil
	})
	return wait, err
}

func VerifiedBackgroundWait(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, runtime db.AgentRuntime) (*protocol.ManagedWait, error) {
	wait, err := TaskBackgroundWait(task)
	if err != nil || wait == nil || !wait.Suspended {
		return nil, err
	}
	a := wait.Address
	if wait.Result == nil || wait.DeliveryID == "" || a.TaskID != util.UUIDToString(task.ID) || a.RuntimeID != util.UUIDToString(runtime.ID) || a.DaemonID != runtime.DaemonID.String || a.Provider != runtime.Provider || a.SessionID != task.SessionID.String || a.WorkDir != task.WorkDir.String || task.ForceFreshSession {
		return nil, fmt.Errorf("%w: original background wait session is unavailable", ErrManagedWaitConflict)
	}
	if err := delegation.ValidateTaskScope(ctx, q, runtime.WorkspaceID, task); err != nil {
		return nil, err
	}
	return wait, nil
}

func consumeResumedWait(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) error {
	wait, err := TaskBackgroundWait(task)
	if err != nil || wait == nil || !wait.Suspended || wait.Consumed {
		return err
	}
	return consumeTaskWait(ctx, q, task, wait)
}

// RecoverBackgroundWaits runs before ordinary orphan recovery. Known journals
// replay their durable outbox; a registration missing locally reports lost.
func (s *TaskService) RecoverBackgroundWaits(ctx context.Context, runtimeID pgtype.UUID, knownIDs []string) error {
	known := make(map[string]bool, len(knownIDs))
	for _, id := range knownIDs {
		known[id] = true
	}
	rows, err := s.Queries.ListRuntimeBackgroundWaitTasks(ctx, runtimeID)
	if err != nil {
		return err
	}
	for _, candidate := range rows {
		if err := s.runInTx(ctx, func(q *db.Queries) error {
			task, err := q.LockTaskSessionPin(ctx, candidate.ID)
			if err != nil {
				return err
			}
			wait, err := TaskBackgroundWait(task)
			if err != nil || wait == nil || wait.Consumed || terminalResourceTask(task.Status) {
				return err
			}
			wait.Suspended = true
			if err := storeTaskWait(ctx, q, task, wait); err != nil {
				return err
			}
			wake := pgtype.Timestamptz{}
			if wait.Result != nil {
				wake = pgtype.Timestamptz{Time: time.Now(), Valid: true}
			}
			task, err = q.SuspendTaskForBackgroundWait(ctx, db.SuspendTaskForBackgroundWaitParams{TaskID: task.ID, WakeAt: wake})
			if err != nil {
				return err
			}
			if wait.Result == nil && !known[wait.Address.TaskID+":"+wait.ID] {
				_, err = completeManagedWait(ctx, q, task, protocol.WaitResult{ID: wait.ID, Address: wait.Address, State: "lost", FinishedAt: time.Now().UTC()})
			}
			return err
		}); err != nil {
			return err
		}
		if updated, err := s.Queries.GetAgentTask(ctx, candidate.ID); err == nil {
			s.NotifyTaskEnqueued(ctx, updated)
		}
	}
	return nil
}

type taskTurnKey struct{}

func WithTaskTurn(ctx context.Context, turn protocol.TaskTurn) context.Context {
	return context.WithValue(ctx, taskTurnKey{}, turn)
}

func staleWaitTurn(ctx context.Context, task db.AgentTaskQueue) bool {
	wait, _ := TaskBackgroundWait(task)
	turn, transport := ctx.Value(taskTurnKey{}).(protocol.TaskTurn)
	return wait != nil && transport && turn.Generation != util.UUIDToString(task.SessionPinGeneration)
}

func suspendCompletedTurn(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, wait *protocol.ManagedWait, branch string) (db.AgentTaskQueue, error) {
	if wait.Suspended && task.Status == "deferred" {
		return task, nil
	}
	turn, transport := ctx.Value(taskTurnKey{}).(protocol.TaskTurn)
	if transport && turn.Generation != wait.Generation {
		return task, ErrManagedWaitConflict
	}
	wait.Suspended = true
	if err := storeTaskWait(ctx, q, task, wait); err != nil {
		return task, err
	}
	wake := pgtype.Timestamptz{}
	if wait.Result != nil {
		wake = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	updated, err := q.SuspendTaskForBackgroundWait(ctx, db.SuspendTaskForBackgroundWaitParams{TaskID: task.ID, BranchName: branch, WakeAt: wake})
	if err != nil {
		return task, err
	}
	if transport && wait.Result == nil && turn.KnownWaitIDs != nil {
		known := false
		for _, id := range turn.KnownWaitIDs {
			known = known || id == wait.ID
		}
		if !known {
			_, err = completeManagedWait(ctx, q, updated, protocol.WaitResult{ID: wait.ID, Address: wait.Address, State: "lost", FinishedAt: time.Now().UTC()})
			if err != nil {
				return task, err
			}
			return q.GetAgentTask(ctx, task.ID)
		}
	}
	return updated, nil
}
