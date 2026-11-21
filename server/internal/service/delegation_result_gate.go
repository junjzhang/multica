package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ResultSuppressedNoAction marks a contracted run whose normal completion
// needs nothing from the dispatcher: no submit, no hand-back.
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
	// An unreadable contract must neither block run completion nor hide a
	// result: treat it as absent so every outcome returns, as without one.
	var state issuecontract.State
	if json.Unmarshal(raw, &state) != nil {
		return resultContractState{}, nil
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

// settleProgressOnlyResults runs inside a completion transaction. On a
// contracted issue, a run that neither submitted nor left the issue blocked
// (the explicit hand-back) needs nothing from its dispatcher, so its returns
// settle here instead of reaching the sweeper. Deciding at completion pins the
// hand-back to this run: the next run on the issue cannot start before this
// commit, so a later status change cannot hide or fake it. Failed runs and
// uncontracted issues always return and never pass through here.
func settleProgressOnlyResults(ctx context.Context, q *db.Queries, completed db.AgentTaskQueue) error {
	if !completed.IssueID.Valid {
		return nil
	}
	issue, err := q.GetIssue(ctx, completed.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Custom statuses never carry blocked behavior; only the built-in key hands back.
	if issue.Status == issuestatus.Blocked {
		return nil
	}
	contract, err := readResultContractState(ctx, q, issue, completed)
	if err != nil || !contract.present || contract.submitted {
		return err
	}
	return q.SettleTaskResultsWithoutNotification(ctx, db.SettleTaskResultsWithoutNotificationParams{
		TaskID: completed.ID,
		Reason: pgtype.Text{String: ResultSuppressedNoAction, Valid: true},
	})
}

func (c resultContractState) addTo(payload map[string]any) {
	if c.present {
		payload["contract_sequence"] = c.sequence
		payload["contract_delivery_count"] = c.deliveries
	}
}
