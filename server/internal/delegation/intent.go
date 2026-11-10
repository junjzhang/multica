package delegation

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func SameManagedSourceScope(ctx context.Context, q *db.Queries, accepted db.IssueDelegationReturn, anchor, caller db.AgentTaskQueue) (bool, error) {
	if accepted.ScopeTaskID.Valid {
		scope, err := ManagedSourceTaskScope(ctx, q, caller, accepted.WorkspaceID)
		return err == nil && scope == accepted.ScopeTaskID, err
	}
	return anchor.IssueID == caller.IssueID && anchor.ChatSessionID == caller.ChatSessionID && anchor.AutopilotRunID == caller.AutopilotRunID, nil
}

// ReturnIntent keeps responsibility provenance separate from a reusable
// physical address. Nil callers means unknown, not an empty accepted set.
type ReturnIntent struct {
	ID            pgtype.UUID
	CallerTaskIDs []pgtype.UUID
}

func IntentForAddress(accepted db.IssueDelegationReturn, address Address) ReturnIntent {
	intent := ReturnIntent{ID: accepted.ID, CallerTaskIDs: []pgtype.UUID{}}
	if address.Kind == KindManagedTask {
		intent.CallerTaskIDs = []pgtype.UUID{parseUUIDOrZero(address.SourceTaskID)}
	}
	return intent
}

func UnionReturns(sets ...[]ReturnIntent) []ReturnIntent {
	var result []ReturnIntent
	indexes := make(map[pgtype.UUID]int)
	for _, set := range sets {
		for _, intent := range set {
			if !intent.ID.Valid {
				continue
			}
			if index, found := indexes[intent.ID]; found {
				result[index].CallerTaskIDs = unionCallerTasks(result[index].CallerTaskIDs, intent.CallerTaskIDs)
				continue
			}
			indexes[intent.ID] = len(result)
			result = append(result, intent)
		}
	}
	return result
}

func unionCallerTasks(left, right []pgtype.UUID) []pgtype.UUID {
	if left == nil || right == nil {
		return nil
	}
	result := make([]pgtype.UUID, 0, len(left)+len(right))
	seen := make(map[pgtype.UUID]bool)
	for _, set := range [][]pgtype.UUID{left, right} {
		for _, id := range set {
			if !seen[id] {
				result = append(result, id)
				seen[id] = true
			}
		}
	}
	return result
}
