package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/delegation"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func resultCallerProvenance(ctx context.Context, q *db.Queries, incoming db.AgentTaskDelegationReturn, address db.IssueDelegationReturn, anchor db.AgentTaskQueue, agent db.Agent) (attribution.Result, bool, error) {
	kind := attribution.EvidenceKind("delegation_result")
	unknown := attribution.OwnerFallback(attribution.Unattributed(kind, incoming.TaskID), agent.OwnerID)
	callers := incoming.CallerTaskIds
	if incoming.TakeoverID.Valid {
		callers = []pgtype.UUID{anchor.ID}
	}
	if callers == nil {
		return unknown, false, nil
	}
	if len(callers) == 0 {
		return unknown, false, resultRefusal("managed_caller_provenance_missing")
	}
	var first db.AgentTaskQueue
	known := true
	for i, id := range callers {
		caller, err := q.GetAgentTask(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return unknown, false, resultRefusal("accepted_caller_missing")
		}
		if err != nil {
			return unknown, false, err
		}
		if caller.AgentID != agent.ID || caller.RuntimeID != address.RuntimeID {
			return unknown, false, resultRefusal("accepted_caller_identity_changed")
		}
		// The original caller's scope was frozen by this very acceptance;
		// quick-create may since have linked that task to its created issue.
		if caller.ID != address.SourceTaskID {
			sameScope, err := delegation.SameManagedSourceScope(ctx, q, address, anchor, caller)
			if err != nil {
				return unknown, false, err
			}
			if !sameScope {
				return unknown, false, resultRefusal("accepted_caller_scope_conflict")
			}
			if err := validateResultSourceScope(ctx, q, caller, address.WorkspaceID); err != nil {
				return unknown, false, err
			}
		}
		if i == 0 {
			first = caller
		} else if caller.OriginatorUserID != first.OriginatorUserID {
			return unknown, false, resultRefusal("accepted_caller_authority_conflict")
		}
		callerKnown, err := inheritedUpstreamKnown(ctx, q, caller.ID, make(map[pgtype.UUID]bool))
		if err != nil {
			return unknown, false, err
		}
		known = known && callerKnown
	}
	lineage := pgtype.UUID{}
	if len(callers) == 1 {
		lineage = first.ID
	}
	resolved := attribution.AgentDelegation(first.OriginatorUserID, first.AccountableUserID, lineage, kind, incoming.TaskID)
	return attribution.OwnerFallback(resolved, agent.OwnerID), known || incoming.TakeoverID.Valid, nil
}

// Inspect durable incoming edges, never a diagnostic bit in task context.
func inheritedUpstreamKnown(ctx context.Context, q *db.Queries, taskID pgtype.UUID, visiting map[pgtype.UUID]bool) (bool, error) {
	if visiting[taskID] {
		return false, nil
	}
	visiting[taskID] = true
	defer delete(visiting, taskID)
	incoming, err := q.GetDelegationResultObligationForTask(ctx, taskID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	edges, loadErr := q.ListConsumedResultObligations(ctx, taskID)
	if loadErr != nil {
		return false, loadErr
	}
	if err == nil {
		edges = append(edges, incoming)
	}
	if len(edges) == 0 {
		task, taskErr := q.GetAgentTask(ctx, taskID)
		return taskErr == nil && !delegation.IsResultTask(task), taskErr
	}
	for _, edge := range edges {
		if edge.TakeoverID.Valid {
			continue
		}
		if edge.CallerTaskIds == nil {
			return false, nil
		}
		for _, caller := range edge.CallerTaskIds {
			// An ordinary caller consuming its own delegated result adds no ancestor.
			if caller == taskID {
				continue
			}
			known, err := inheritedUpstreamKnown(ctx, q, caller, visiting)
			if err != nil || !known {
				return known, err
			}
		}
	}
	return true, nil
}
