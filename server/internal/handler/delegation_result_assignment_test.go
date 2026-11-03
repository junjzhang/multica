package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDelegationResultAndAssignmentKeepDistinctInputSlots(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	for _, kind := range []string{"agent", "squad"} {
		for _, first := range []string{"assignment", "result"} {
			t.Run(kind+"/"+first, func(t *testing.T) {
				source, result, _ := resultFixture(t, "slots-"+kind+"-"+first)
				dbfx.Exec(t, `UPDATE agent SET max_concurrent_tasks=3 WHERE id=$1`, source.AgentID)
				issueID := uuidToString(source.IssueID)
				assignee := uuidToString(source.AgentID)
				if kind == "squad" {
					assignee = dbfx.Squad(t, "result slot squad", assignee)
				}
				var dispatcher string
				var dispatchToken string
				if first == "result" {
					dispatcher, _, dispatchToken = managedDispatcherFixture(t, "new-slot-dispatcher-"+kind)
					dbfx.Exec(t, `INSERT INTO agent_invocation_target(agent_id,target_type,target_id) VALUES($1,'workspace',$2) ON CONFLICT DO NOTHING`, source.AgentID, testWorkspaceID)
				}
				assign := func() {
					t.Helper()
					body := map[string]any{"assignee_type": kind, "assignee_id": assignee}
					if dispatchToken != "" {
						response := serveAuthenticated(t, dispatchToken, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch, "/api/issues/"+issueID, body, map[string]string{"id": issueID}, nil)
						if response.Code != http.StatusOK {
							t.Fatalf("managed assignment: %d %s", response.Code, response.Body.String())
						}
						return
					}
					testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, body), "id", issueID)).Want(http.StatusOK)
				}
				notify := func() {
					t.Helper()
					finishResultFixture(t, result)
					if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
						t.Fatal(err)
					}
				}
				if first == "assignment" {
					assign()
					notify()
				} else {
					notify()
					assign()
				}
				rows, err := testHandler.Queries.ListTaskResultNotifications(ctx, db.ListTaskResultNotificationsParams{TaskID: result.ID, WorkspaceID: parseUUID(testWorkspaceID)})
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || !rows[0].NotificationTaskID.Valid {
					t.Fatal("ordinary slot prevented result materialization")
				}
				typed, err := testHandler.Queries.GetAgentTask(ctx, rows[0].NotificationTaskID)
				if err != nil {
					t.Fatal(err)
				}
				ordinary, err := testHandler.Queries.GetPendingAssignmentTaskForIssueAndAgent(ctx, db.GetPendingAssignmentTaskForIssueAndAgentParams{IssueID: source.IssueID, AgentID: source.AgentID})
				if err != nil {
					t.Fatal(err)
				}
				if ordinary.ID == typed.ID || delegation.IsResultTask(ordinary) || ordinary.IsLeaderTask != (kind == "squad") || ordinary.CommentThreadID.Valid || typed.CommentThreadID.Valid {
					t.Fatal("assignment and result inputs were mixed or given a fabricated thread")
				}
				if dispatcher != "" {
					accepted, err := testHandler.Queries.GetIssueDelegationReturnForDispatcher(ctx, db.GetIssueDelegationReturnForDispatcherParams{WorkspaceID: parseUUID(testWorkspaceID), IssueID: source.IssueID, DispatcherAgentID: parseUUID(dispatcher)})
					if err != nil {
						t.Fatal(err)
					}
					requireTaskReturns(t, uuidToString(ordinary.ID), accepted)
					for _, link := range taskReturnIDs(t, uuidToString(typed.ID)) {
						if link.ID == accepted.ID {
							t.Fatal("new assignment responsibility landed on result-only input")
						}
					}
				}
				claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, source.RuntimeID)
				if err != nil || claimed == nil {
					t.Fatalf("first claim: %v", err)
				}
				second, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, source.RuntimeID)
				if err != nil || second != nil {
					t.Fatalf("same scope was claimed concurrently despite capacity 3: task=%v err=%v", second, err)
				}
				if _, err := testHandler.TaskService.CancelTask(ctx, claimed.ID); err != nil {
					t.Fatal(err)
				}
				second, err = testHandler.TaskService.ClaimTaskForRuntime(ctx, source.RuntimeID)
				if err != nil || second == nil || second.ID == claimed.ID {
					t.Fatalf("remaining input failed to advance after first settled: %v", err)
				}
				t.Logf("distinct ordinary=%s result=%s retain shared claim serialization", uuidToString(ordinary.ID), uuidToString(typed.ID))
			})
		}
	}
}

func TestDelegationOrdinaryRetryDoesNotTreatResultAsSuccessor(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, _ := resultFixture(t, "retry-result-slot")
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	parent := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": uuidToString(source.IssueID), "runtime_id": uuidToString(source.RuntimeID), "status": "failed", "failure_reason": "runtime_recovery", "attempt": int32(1), "max_attempts": int32(2)})
	failed, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(parent))
	if err != nil {
		t.Fatal(err)
	}
	child, err := testHandler.TaskService.MaybeRetryFailedTask(ctx, failed)
	if err != nil || child == nil {
		t.Fatalf("typed result was treated as retry successor: %v", err)
	}
	if child.RetryOfTaskID != failed.ID || delegation.IsResultTask(*child) {
		t.Fatal("ordinary retry changed input identity")
	}
	// A second ordinary insert still hits the classified ordinary unique slot.
	_, err = testHandler.TaskService.MaybeRetryFailedTask(ctx, failed)
	if err != nil {
		t.Fatalf("repeated retry decision: %v", err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1`, failed.ID); n != 1 {
		t.Fatal(fmt.Sprintf("duplicate ordinary retries: %d", n))
	}
}
