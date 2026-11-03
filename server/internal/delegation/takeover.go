package delegation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PrepareTakeoverTarget proves a selected live target and reuses an immutable
// physical address when possible. The caller serializes the issue's current
// relationship and owns the transaction; no routing is changed here.
func PrepareTakeoverTarget(ctx context.Context, q *db.Queries, ws, issue, dispatcher pgtype.UUID, kind Kind, selector pgtype.UUID) (db.IssueDelegationReturn, db.AgentTaskQueue, error) {
	candidate := db.IssueDelegationReturn{WorkspaceID: ws, IssueID: issue, DispatcherAgentID: dispatcher, ReturnKind: string(kind)}
	var source db.AgentTaskQueue
	switch kind {
	case KindManagedTask:
		task, runtime, err := recheckManagedSourceLocked(ctx, q, ws, dispatcher, selector)
		if err != nil {
			return candidate, source, err
		}
		scope, err := ManagedSourceTaskScope(ctx, q, task, ws)
		if err != nil {
			return candidate, source, err
		}
		source = task
		candidate.SourceTaskID, candidate.RuntimeID = task.ID, runtime.ID
		candidate.Provider = pgtype.Text{String: runtime.Provider, Valid: true}
		candidate.SessionID, candidate.WorkDir, candidate.ScopeTaskID = task.SessionID, task.WorkDir, scope
	case KindExternalSession:
		_, err := ResolveExternal(ctx, q, util.UUIDToString(ws), util.UUIDToString(dispatcher), util.UUIDToString(selector))
		if err != nil {
			return candidate, source, err
		}
		candidate.ExternalSessionID = selector
	default:
		return candidate, source, ErrReturnInvalid
	}
	versions, err := q.ListIssueDelegationReturns(ctx, db.ListIssueDelegationReturnsParams{WorkspaceID: ws, IssueID: issue})
	if err != nil {
		return candidate, source, err
	}
	var matched db.IssueDelegationReturn
	for _, prior := range versions {
		if prior.DispatcherAgentID == dispatcher && samePhysicalReturn(prior, candidate) && (!matched.ID.Valid || prior.IsCurrent) {
			matched = prior
		}
	}
	if matched.ID.Valid {
		if kind == KindManagedTask {
			if err := validateTakeoverScopeReuse(ctx, q, matched, candidate, source); err != nil {
				return candidate, source, err
			}
		}
		return matched, source, nil
	}

	return candidate, source, nil
}

func samePhysicalReturn(a, b db.IssueDelegationReturn) bool {
	if a.ReturnKind != b.ReturnKind {
		return false
	}
	if a.ReturnKind == string(KindExternalSession) {
		return a.ExternalSessionID == b.ExternalSessionID
	}
	return a.RuntimeID == b.RuntimeID && a.Provider == b.Provider && a.SessionID == b.SessionID && a.WorkDir == b.WorkDir
}

func validateTakeoverScopeReuse(ctx context.Context, q *db.Queries, prior, candidate db.IssueDelegationReturn, source db.AgentTaskQueue) error {
	original, err := q.GetAgentTask(ctx, prior.SourceTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: original source unavailable", ErrReturnInvalid)
	}
	if err != nil {
		return err
	}
	if original.AgentID != prior.DispatcherAgentID {
		return ErrReturnInvalid
	}
	if prior.ScopeTaskID.Valid {
		if err := ValidateOriginalTaskScope(ctx, q, prior); err != nil {
			return err
		}
		if prior.ScopeTaskID != candidate.ScopeTaskID {
			return fmt.Errorf("%w: original task scope differs", ErrReturnConflict)
		}
		return nil
	}
	if candidate.ScopeTaskID.Valid || original.IssueID != source.IssueID || original.ChatSessionID != source.ChatSessionID || original.AutopilotRunID != source.AutopilotRunID {
		return fmt.Errorf("%w: original scope differs", ErrReturnConflict)
	}
	return requireTaskScope(ctx, q, prior.WorkspaceID, original)
}
