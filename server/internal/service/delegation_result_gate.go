package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ResultSuppressedNoAction marks a contracted run whose normal completion
// needs nothing from the dispatcher: no submit, no failure, no hand-back.
const ResultSuppressedNoAction = "suppressed_no_dispatcher_action"

// resultContractState is the contract snapshot a result return carries so a
// dispatcher can order returns that arrive out of order.
type resultContractState struct {
	present    bool
	sequence   int
	deliveries int
	submitted  bool
}

func readResultContractState(ctx context.Context, q *db.Queries, issue db.Issue, outcome db.AgentTaskQueue) (resultContractState, error) {
	raw, err := q.GetIssueContract(ctx, db.GetIssueContractParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return resultContractState{}, nil
	}
	if err != nil {
		return resultContractState{}, err
	}
	var state issuecontract.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return resultContractState{}, err
	}
	if state.Current() == nil {
		return resultContractState{}, nil
	}
	chain, err := executionChainIDs(ctx, q, outcome)
	if err != nil {
		return resultContractState{}, err
	}
	submitted := slices.ContainsFunc(state.Deliveries, func(d issuecontract.Delivery) bool {
		return d.TaskID != "" && slices.Contains(chain, d.TaskID)
	})
	return resultContractState{present: true, sequence: state.Sequence, deliveries: len(state.Deliveries), submitted: submitted}, nil
}

// executionChainIDs returns the outcome and every attempt it retried, so a
// submit made by an earlier attempt still counts for the final outcome.
func executionChainIDs(ctx context.Context, q *db.Queries, outcome db.AgentTaskQueue) ([]string, error) {
	ids := []string{util.UUIDToString(outcome.ID)}
	parent := outcome.RetryOfTaskID
	for parent.Valid && len(ids) < 64 {
		task, err := q.GetAgentTask(ctx, parent)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, util.UUIDToString(task.ID))
		parent = task.RetryOfTaskID
	}
	return ids, nil
}

// needsDispatcher decides whether a run outcome on a contracted issue wakes
// the dispatcher. Without a contract every outcome returns, as before. With
// one, only a submit by this run, a failed run, or an explicit hand-back (the
// issue left blocked) does. Custom statuses never carry blocked behavior, so
// only the built-in key counts.
func (c resultContractState) needsDispatcher(issueStatus, outcomeStatus string) bool {
	return !c.present || c.submitted || outcomeStatus == "failed" || issueStatus == issuestatus.Blocked
}

func (c resultContractState) addTo(payload map[string]any) {
	if c.present {
		payload["contract_sequence"] = c.sequence
		payload["contract_delivery_count"] = c.deliveries
	}
}
