package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func startResultHandlingTurn(t *testing.T, result db.AgentTaskQueue) (db.AgentTaskQueue, AgentTaskResponse) {
	t.Helper()
	ctx := context.Background()
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	rows, err := testHandler.Queries.ListTaskResultNotifications(ctx, db.ListTaskResultNotificationsParams{TaskID: result.ID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil || len(rows) != 1 || !rows[0].NotificationTaskID.Valid {
		t.Fatalf("result notification: %v %+v", err, rows)
	}
	task, err := testHandler.Queries.GetAgentTask(ctx, rows[0].NotificationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newOwnerDaemonRequest("POST", "/claim", nil), "runtimeId", uuidToString(task.RuntimeID))
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	var claim struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(200).JSON(&claim)
	if claim.Task == nil || claim.Task.ID != uuidToString(task.ID) || claim.Task.DelegationResult == nil {
		t.Fatal("expected strict result claim")
	}
	task, _ = testHandler.Queries.GetAgentTask(ctx, task.ID)
	testutil.Call(t, testHandler.StartTask, startClaimRequest(uuidToString(task.ID), uuidToString(task.RuntimeID), task.DispatchedAt.Time)).Want(200)
	address := claim.Task.DelegationResult
	testutil.Call(t, testHandler.PinTaskSession, withURLParam(newOwnerDaemonRequest("POST", "/session", protocol.SessionPinRequest{SessionID: address.SessionID, WorkDir: address.WorkDir, Generation: claim.Task.SessionPinGeneration}), "taskId", uuidToString(task.ID))).Want(204)
	task, _ = testHandler.Queries.GetAgentTask(ctx, task.ID)
	return task, *claim.Task
}

func TestDelegationTypedTerminalCannotReplaceFrozenAddress(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	for _, mode := range []string{"complete-valid", "complete-drift", "complete-no-session", "complete-no-directory", "fail-drift", "cancel-retire"} {
		t.Run(mode, func(t *testing.T) {
			_, result, _ := resultFixture(t, "terminal-"+mode)
			finishResultFixture(t, result)
			original, _ := testHandler.Queries.GetAgentTask(ctx, result.ID)
			task, claim := startResultHandlingTurn(t, result)
			address := claim.DelegationResult
			body := map[string]any{"output": "handled outcome", "session_id": address.SessionID, "work_dir": address.WorkDir}
			callback := testHandler.CompleteTask
			switch mode {
			case "complete-drift":
				body["session_id"], body["work_dir"] = "wrong-B", "/wrong-B"
			case "complete-no-session":
				delete(body, "session_id")
			case "complete-no-directory":
				delete(body, "work_dir")
			case "fail-drift":
				callback = testHandler.FailTask
				body = map[string]any{"error": "input outcome unknown", "failure_reason": protocol.DelegationResultInputUnknown, "session_id": "wrong-B", "work_dir": "/wrong-B"}
			case "cancel-retire":
				if _, err := testHandler.TaskService.CancelTask(ctx, task.ID); err != nil {
					t.Fatal(err)
				}
				callback = testHandler.AckTaskCancelled
				body = map[string]any{"retired_session_id": address.SessionID, "durable_work_dir": "/unconfirmed-durable"}
			}
			testutil.Call(t, callback, withURLParam(newOwnerDaemonRequest("POST", "/terminal", body), "taskId", uuidToString(task.ID))).Want(200)
			after, err := testHandler.Queries.GetAgentTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.SessionID.String != address.SessionID || after.WorkDir.String != address.WorkDir || after.DurableWorkDir.Valid {
				t.Fatalf("terminal replaced frozen location: session=%q dir=%q durable=%v", after.SessionID.String, after.WorkDir.String, after.DurableWorkDir.Valid)
			}
			if mode == "cancel-retire" {
				if after.Status != "cancelled" {
					t.Fatal("cancellation fact was rewritten")
				}
			} else if mode == "complete-valid" {
				if after.Status != "completed" {
					t.Fatalf("verified success refused: %s", after.Status)
				}
			} else if after.Status != "failed" || after.FailureReason.String != protocol.DelegationResultInputUnknown {
				t.Fatalf("contradictory or incomplete success was accepted: %s/%s", after.Status, after.FailureReason.String)
			}
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Fatal(err)
			}
			same, _ := testHandler.Queries.GetAgentTask(ctx, result.ID)
			if string(same.Result) != string(original.Result) {
				t.Fatal("notification failure rewrote original executor result")
			}
			var id string
			dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, result.ID).Scan(&id)
			if id != uuidToString(task.ID) {
				t.Fatal("unknown input created another notification")
			}
		})
	}
}
