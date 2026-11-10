package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const ResultTaskType = "delegation_result"

type ResultTaskContext struct {
	Type   string                        `json:"type"`
	Result protocol.DelegationResultTurn `json:"delegation_result"`
}

func IsResultTask(task db.AgentTaskQueue) bool {
	var value struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(task.Context, &value) == nil && value.Type == ResultTaskType
}

func ResultTurn(task db.AgentTaskQueue) (*protocol.DelegationResultTurn, error) {
	if !IsResultTask(task) {
		return nil, nil
	}
	var value ResultTaskContext
	if err := json.Unmarshal(task.Context, &value); err != nil {
		return nil, err
	}
	r := &value.Result
	if r.ResultTaskID == "" || r.DelegationReturnID == "" || r.SourceTaskID == "" ||
		r.RuntimeID == "" || r.Provider == "" || r.SessionID == "" || r.WorkDir == "" {
		return nil, errors.New("result handling task has no complete frozen address")
	}
	return r, nil
}

// ManagedSourceTaskScope freezes the physical namespace before accepting new
// work. A typed source proves its original namespace through its current
// durable notification; the new return copies that fact and owns it thereafter.
func ManagedSourceTaskScope(ctx context.Context, q *db.Queries, source db.AgentTaskQueue, workspace pgtype.UUID) (pgtype.UUID, error) {
	if source.IssueID.Valid || source.ChatSessionID.Valid || source.AutopilotRunID.Valid {
		return pgtype.UUID{}, nil
	}
	if !IsResultTask(source) {
		return source.ID, nil
	}
	link, err := q.GetDelegationResultObligationForTask(ctx, source.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("%w: result source has no durable notification", ErrReturnInvalid)
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load result source notification: %w", err)
	}
	accepted, err := q.GetDelegationReturnByID(ctx, link.DelegationReturnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("%w: result source return is missing", ErrReturnInvalid)
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load result source return: %w", err)
	}
	projected, err := ResultTurn(source)
	if err != nil || projected == nil || accepted.WorkspaceID != workspace || accepted.ReturnKind != string(KindManagedTask) ||
		source.AgentID != accepted.DispatcherAgentID || source.RuntimeID != accepted.RuntimeID || source.SessionID != accepted.SessionID || source.WorkDir != accepted.WorkDir ||
		projected.ResultTaskID != util.UUIDToString(link.TaskID) || projected.DelegationReturnID != util.UUIDToString(accepted.ID) || projected.SourceTaskID != util.UUIDToString(accepted.SourceTaskID) || projected.ScopeTaskID != util.UUIDToString(accepted.ScopeTaskID) {
		return pgtype.UUID{}, fmt.Errorf("%w: result source scope mismatch", ErrReturnInvalid)
	}
	if err := ValidateOriginalTaskScope(ctx, q, accepted); err != nil {
		return pgtype.UUID{}, err
	}
	return accepted.ScopeTaskID, nil
}

func ValidateOriginalTaskScope(ctx context.Context, q *db.Queries, accepted db.IssueDelegationReturn) error {
	if !accepted.ScopeTaskID.Valid {
		return fmt.Errorf("%w: original task scope is missing", ErrReturnInvalid)
	}
	root, err := q.LockAgentTaskForDelegation(ctx, accepted.ScopeTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: original task scope unavailable", ErrReturnInvalid)
	}
	if err != nil {
		return fmt.Errorf("load original task scope: %w", err)
	}
	if root.AgentID != accepted.DispatcherAgentID || IsResultTask(root) {
		return fmt.Errorf("%w: original task scope mismatch", ErrReturnInvalid)
	}
	actor, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: root.AgentID, WorkspaceID: accepted.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: original task scope workspace mismatch", ErrReturnInvalid)
	}
	if err != nil {
		return fmt.Errorf("load original scope agent: %w", err)
	}
	if actor.ID != root.AgentID {
		return fmt.Errorf("%w: original task scope workspace mismatch", ErrReturnInvalid)
	}
	return nil
}
