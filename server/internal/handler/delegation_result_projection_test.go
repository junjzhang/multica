package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func completeResultHandlingTurn(t *testing.T, result db.AgentTaskQueue) {
	t.Helper()
	task, claim := startResultHandlingTurn(t, result)
	address := claim.DelegationResult
	message := queryString(t, `SELECT notification_delivery_id::text FROM agent_task_delegation_return WHERE notification_task_id=$1`, task.ID)
	ackManagedResult(t, task, message, "handled", http.StatusOK)
	testutil.Call(t, testHandler.CompleteTask, withURLParam(newOwnerDaemonRequest("POST", "/complete", map[string]string{"output": "Processed the exact delegated outcome", "session_id": address.SessionID, "work_dir": address.WorkDir}), "taskId", uuidToString(task.ID))).Want(200)
}

func requireFinalNotificationRead(t *testing.T, issueID, oldID, leafID string) {
	t.Helper()
	var runs []AgentTaskResponse
	testutil.Call(t, testHandler.ListTasksByIssue, withURLParam(newRequest("GET", "/api/issues/"+issueID+"/task-runs", nil), "id", issueID)).Want(http.StatusOK).JSON(&runs)
	foundOld, foundLeaf := false, false
	for _, run := range runs {
		if run.ID == oldID {
			foundOld = true
			if run.ResultNotifications == nil || len(*run.ResultNotifications) != 0 {
				t.Fatalf("non-notifying old attempt displayed pending: %+v", run.ResultNotifications)
			}
		}
		if run.ID == leafID {
			foundLeaf = true
			if run.ResultNotifications == nil || len(*run.ResultNotifications) != 1 || (*run.ResultNotifications)[0].Status != "received" {
				t.Fatalf("final outcome receipt missing: %+v", run.ResultNotifications)
			}
		}
	}
	if !foundOld || !foundLeaf {
		t.Fatal("task history lost old execution or final leaf")
	}
}

func TestResultNotificationReadSkipsRetryIntermediateAttempt(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	_, parent, _ := resultFixture(t, "notification-retry-read")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),max_attempts=3 WHERE id=$1`, parent.ID)
	if _, err := testHandler.TaskService.FailTask(service.WithTerminalReport(ctx), parent.ID, "temporary provider disconnect", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatal(err)
	}
	leaf, err := testHandler.Queries.GetAutomaticRetryChild(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, leaf)
	completeResultHandlingTurn(t, leaf)
	requireFinalNotificationRead(t, uuidToString(parent.IssueID), uuidToString(parent.ID), uuidToString(leaf.ID))
}

func TestResultNotificationReadSkipsCommentChangeCancelledAttempt(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	_, sourceID, token := managedDispatcherFixture(t, "notification-edit-read")
	source, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(sourceID))
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='notification-edit-read' WHERE id=$1`, source.RuntimeID)
	target, _ := executableTarget(t, "notification-edit-target")
	issueID := dbfx.Issue(t, "notification edit result")
	content := fmt.Sprintf("[@Worker](mention://agent/%s) first request", target)
	code, commentID, body := intentComment(t, token, issueID, content, "")
	if code != 201 {
		t.Fatalf("create comment: %d %s", code, body)
	}
	oldID := latestTaskIDFor(t, issueID, target)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch, "/api/issues/"+issueID+"/comments/"+commentID, map[string]any{"content": content + " revised"}, map[string]string{"id": issueID, "commentId": commentID}, nil)
	if w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	leafID := latestTaskIDFor(t, issueID, target)
	if leafID == oldID {
		t.Fatal("comment edit did not create its replacement run")
	}
	old, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(oldID))
	if old.Status != "cancelled" {
		t.Fatal("old task was not cancelled by actual comment edit")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, source.ID)
	leaf, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(leafID))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, leaf)
	completeResultHandlingTurn(t, leaf)
	requireFinalNotificationRead(t, issueID, oldID, leafID)
}
