package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func continuationRun(t *testing.T, source db.AgentTaskQueue, suffix string) (db.AgentTaskQueue, string) {
	t.Helper()
	id := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{
		"runtime_id": source.RuntimeID, "issue_id": source.IssueID, "status": "running",
		"session_id": "continued-" + suffix, "work_dir": "/work/continued-" + suffix,
		"resource_control_required": true, "session_pin_confirmed": true,
		"session_pin_generation": testutil.Raw("gen_random_uuid()"),
		"originator_user_id":     source.OriginatorUserID, "accountable_user_id": source.AccountableUserID,
	})
	run, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return run, taskTokenFixture(t, uuidToString(source.AgentID), id)
}

func dispatchContinuation(t *testing.T, token string, result db.AgentTaskQueue, worker string, want int) {
	t.Helper()
	code, _, body := intentComment(t, token, uuidToString(result.IssueID), fmt.Sprintf("[@Reviewer](mention://agent/%s) review continued work", worker), "")
	if code != want {
		t.Fatalf("continuation dispatch: %d, want %d: %s", code, want, body)
	}
}

func currentContinuationReturn(t *testing.T, original db.IssueDelegationReturn) db.IssueDelegationReturn {
	t.Helper()
	current, err := testHandler.Queries.GetIssueDelegationReturnForDispatcher(context.Background(), db.GetIssueDelegationReturnForDispatcherParams{
		WorkspaceID: original.WorkspaceID, IssueID: original.IssueID, DispatcherAgentID: original.DispatcherAgentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestManagedReturnContinuationForwardsLateResultOnce(t *testing.T) {
	ctx := context.Background()
	source, result, original := resultFixture(t, "managed-continuation")
	worker, _ := executableTarget(t, "continued reviewer")
	current := source
	var next db.IssueDelegationReturn
	// A result may arrive after several failed resume attempts. Only the
	// verified chain connects these addresses; source task pointers stay frozen.
	for i := range 2 {
		run, token := continuationRun(t, current, fmt.Sprintf("hop-%d", i))
		dispatchContinuation(t, token, result, worker, http.StatusCreated)
		next = currentContinuationReturn(t, original)
		if next.SourceTaskID != run.ID || !next.ContinuedFromReturnID.Valid || next.ID == original.ID {
			t.Fatalf("continuation not versioned: %+v", next)
		}
		before := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, result.IssueID)
		dispatchContinuation(t, token, result, worker, http.StatusCreated)
		if countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, result.IssueID) != before {
			t.Fatal("same-session retry rotated return again")
		}
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, run.ID)
		current = run
	}
	finishResultFixture(t, result)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: original.ID})
	if err != nil || !link.NotificationTaskID.Valid || link.DeliveryReturnID != next.ID {
		t.Fatalf("late result did not forward: %+v %v", link, err)
	}
	turn, err := testHandler.Queries.GetAgentTask(ctx, link.NotificationTaskID)
	if err != nil || turn.SessionID != current.SessionID || turn.WorkDir != current.WorkDir {
		t.Fatalf("wrong continuation session: %+v %v", turn, err)
	}
	verified, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, turn)
	if err != nil || verified == nil || verified.DelegationReturnID != uuidToString(next.ID) {
		t.Fatalf("forwarded result cannot verify: %+v %v", verified, err)
	}
	if n := countValue(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='delegation_result_forwarded' AND details->>'result_task_id'=$2`, result.IssueID, uuidToString(result.ID)); n != 1 {
		t.Fatalf("forward audit count = %d, want 1", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_mailbox_message WHERE payload->>'result_task_id'=$1`, uuidToString(result.ID)); n != 1 {
		t.Fatalf("result delivered %d times", n)
	}
	mailboxReturn, err := testHandler.Queries.GetMailboxResultReturn(ctx, db.GetMailboxResultReturnParams{MessageID: link.NotificationDeliveryID, WorkspaceID: original.WorkspaceID})
	if err != nil || mailboxReturn.ID != next.ID {
		t.Fatalf("mailbox did not resolve frozen delivery address: %+v %v", mailboxReturn, err)
	}
	// This actual daemon claim/start/ACK path also verifies physical pin and
	// mailbox ownership, rather than only inspecting the queue projection.
	completeResultHandlingTurn(t, result)
	if old, err := testHandler.Queries.GetDelegationReturnByID(ctx, original.ID); err != nil || old.SourceTaskID != source.ID || old.SessionID != original.SessionID || old.IsCurrent {
		t.Fatalf("original history changed: %+v %v", old, err)
	}
}

func TestManagedReturnContinuationPreservesMaterializedHistory(t *testing.T) {
	ctx := context.Background()
	source, result, original := resultFixture(t, "continuation-history")
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: original.ID})
	if err != nil || !link.NotificationTaskID.Valid {
		t.Fatalf("no-continuation result was lost: %+v %v", link, err)
	}
	// A queued result consumer occupies the source scope and prevents a new
	// session from taking it. Even after it terminates, its history is frozen.
	_, token := continuationRun(t, source, "history")
	worker, _ := executableTarget(t, "history-reviewer")
	dispatchContinuation(t, token, result, worker, http.StatusConflict)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, link.NotificationTaskID)
	dispatchContinuation(t, token, result, worker, http.StatusCreated)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	after, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: original.ID})
	if err != nil || after.NotificationTaskID != link.NotificationTaskID || after.DeliveryReturnID != original.ID {
		t.Fatalf("materialized history retargeted: %+v %v", after, err)
	}
}

func TestManagedReturnContinuationRejectsUnsafeSources(t *testing.T) {
	for _, kind := range []string{"old-active", "concurrent", "other-issue", "unconfirmed-pin", "same-task"} {
		t.Run(kind, func(t *testing.T) {
			source, result, original := resultFixture(t, "unsafe-"+kind)
			run, token := continuationRun(t, source, "unsafe-"+kind)
			switch kind {
			case "old-active":
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, source.ID)
			case "concurrent":
				continuationRun(t, source, "competitor")
			case "other-issue":
				other := dbfx.Issue(t, "another source scope")
				dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=$2 WHERE id=$1`, run.ID, other)
			case "unconfirmed-pin":
				dbfx.Exec(t, `UPDATE agent_task_queue SET session_pin_confirmed=false WHERE id=$1`, run.ID)
			case "same-task":
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',session_id='repinned-same-task' WHERE id=$1`, source.ID)
				token = taskTokenFixture(t, uuidToString(source.AgentID), uuidToString(source.ID))
			}
			worker, _ := executableTarget(t, "unsafe-reviewer-"+kind)
			dispatchContinuation(t, token, result, worker, http.StatusConflict)
			if current := currentContinuationReturn(t, original); current.ID != original.ID {
				t.Fatal("refused continuation changed current return")
			}
			if countValue(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='delegation_return_continued'`, result.IssueID) != 0 {
				t.Fatal("refused continuation left an audit/version")
			}
		})
	}
}
