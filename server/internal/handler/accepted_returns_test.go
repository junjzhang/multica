package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestAcceptedReturnsVisibleBeforeOutcome(t *testing.T) {
	_, run, address := resultFixture(t, "accepted-returns-live")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1`, run.ID)
	emptyID := dbfx.Task(t, uuidToString(run.AgentID), testutil.Cols{"issue_id": uuidToString(run.IssueID), "runtime_id": uuidToString(run.RuntimeID), "status": "running"})
	var rows []map[string]json.RawMessage
	testutil.Call(t, testHandler.ListTasksByIssue, withURLParam(newRequest("GET", "/api/issues/runs", nil), "id", uuidToString(run.IssueID))).Want(http.StatusOK).JSON(&rows)
	seen := map[string]bool{}
	for _, row := range rows {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatal(err)
		}
		if id != uuidToString(run.ID) && id != emptyID {
			continue
		}
		seen[id] = true
		raw, present := row["accepted_returns"]
		if !present || string(raw) == "null" {
			t.Fatalf("running task %s has no authoritative return context", id)
		}
		var returns []struct {
			ReturnID         string `json:"return_id"`
			RecipientAgentID string `json:"recipient_agent_id"`
		}
		if err := json.Unmarshal(raw, &returns); err != nil {
			t.Fatal(err)
		}
		if id == emptyID {
			if len(returns) != 0 {
				t.Fatalf("unrelated run inherited returns: %s", raw)
			}
			continue
		}
		if len(returns) != 1 || returns[0].ReturnID != uuidToString(address.ID) || returns[0].RecipientAgentID != uuidToString(address.DispatcherAgentID) {
			t.Fatalf("accepted return missing before result: %s", raw)
		}
		if string(row["result_notifications"]) != "[]" {
			t.Fatalf("test requires no materialized notification: %s", row["result_notifications"])
		}
	}
	if !seen[uuidToString(run.ID)] || !seen[emptyID] {
		t.Fatalf("missing task rows: %v", seen)
	}
}

type unavailableAcceptedReturnsDB struct{ db.DBTX }

func (f unavailableAcceptedReturnsDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if strings.Contains(query, "-- name: ListAcceptedTaskReturns") {
		return nil, errors.New("accepted return read unavailable")
	}
	return f.DBTX.Query(ctx, query, args...)
}

func TestAcceptedReturnsReadFailureIsNotEmpty(t *testing.T) {
	_, run, _ := resultFixture(t, "accepted-returns-unavailable")
	h := *testHandler
	h.Queries = db.New(unavailableAcceptedReturnsDB{testPool})
	testutil.Call(t, h.ListTasksByIssue, withURLParam(newRequest("GET", "/api/issues/runs", nil), "id", uuidToString(run.IssueID))).Want(http.StatusInternalServerError)

	dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, run.ID)
	run, _ = testHandler.Queries.GetAgentTask(context.Background(), run.ID)
	runtime, _ := testHandler.Queries.GetAgentRuntime(context.Background(), run.RuntimeID)
	req := newOwnerDaemonRequest("POST", "/claim", nil)
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	resp, _, _, _, _, failure := h.buildClaimedTaskResponse(req, &run, runtime, uuidToString(runtime.ID), testWorkspaceID)
	if failure == nil || resp.AcceptedReturns != nil {
		t.Fatalf("unavailable read became known context: failure=%+v returns=%+v", failure, resp.AcceptedReturns)
	}
}

func TestClaimCarriesAcceptedReturns(t *testing.T) {
	_, run, address := resultFixture(t, "accepted-returns-claim")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, run.ID)
	run, _ = testHandler.Queries.GetAgentTask(context.Background(), run.ID)
	runtime, _ := testHandler.Queries.GetAgentRuntime(context.Background(), run.RuntimeID)
	req := newOwnerDaemonRequest("POST", "/claim", nil)
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	resp, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(req, &run, runtime, uuidToString(runtime.ID), testWorkspaceID)
	if failure != nil || resp.AcceptedReturns == nil || len(*resp.AcceptedReturns) != 1 || (*resp.AcceptedReturns)[0].ReturnID != uuidToString(address.ID) {
		t.Fatalf("claim lost accepted return: failure=%+v returns=%+v", failure, resp.AcceptedReturns)
	}
}
