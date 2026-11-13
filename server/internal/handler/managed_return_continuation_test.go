package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
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

func TestManagedReturnContinuationReceivesNewResult(t *testing.T) {
	ctx := context.Background()
	source, oldResult, original := resultFixture(t, "continuation-new-result")
	run, token := continuationRun(t, source, "new-result")
	worker, _ := executableTarget(t, "new-session-reviewer")
	dispatchContinuation(t, token, oldResult, worker, http.StatusCreated)
	current := currentContinuationReturn(t, original)
	result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, uuidToString(oldResult.IssueID), worker)))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, run.ID)
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: current.ID})
	if err != nil || link.DeliveryReturnID != current.ID || !link.NotificationTaskID.Valid {
		t.Fatalf("new delegation did not return to continuation: %+v %v", link, err)
	}
	completeResultHandlingTurn(t, result)
	if n := countValue(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='delegation_result_forwarded'`, result.IssueID); n != 0 {
		t.Fatalf("new-version outcome incorrectly counted as forwarded: %d", n)
	}
}

func TestManagedReturnContinuationRetainsPendingForwardResources(t *testing.T) {
	ctx := context.Background()
	source, oldResult, original := resultFixture(t, "continuation-retention")
	run, token := continuationRun(t, source, "retention")
	worker, _ := executableTarget(t, "retention-reviewer")
	dispatchContinuation(t, token, oldResult, worker, http.StatusCreated)
	current := currentContinuationReturn(t, original)
	newResultID := latestTaskIDFor(t, uuidToString(oldResult.IssueID), worker)
	// The new request is withdrawn and accepted while the original review is
	// still running. The continuation must retain resources for that old input.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='cancelled',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, newResultID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, run.ID)
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, oldResult.IssueID)
	req := protocol.ResourceReleaseRequest{Kind: "workdir", SourceTaskID: uuidToString(run.ID), ResourcePath: run.WorkDir.String, WorkDir: run.WorkDir.String}
	decision, err := testHandler.TaskService.ResourceRelease(ctx, original.WorkspaceID, "result-continuation-retention", req)
	if err != nil || decision.ReleaseAllowed || decision.Reason != "delegated_execution_active" {
		t.Fatalf("pending forwarded review lost its future resources: %+v %v", decision, err)
	}
	deps, err := testHandler.Queries.ListReturnResultDependencies(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, dep := range deps {
		found = found || dep.AgentTaskDelegationReturn.TaskID == oldResult.ID
	}
	if !found {
		t.Fatal("continuation retention omitted pending ancestor outcome")
	}
	finishResultFixture(t, oldResult)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	completeResultHandlingTurn(t, oldResult)
	decision, err = testHandler.TaskService.ResourceRelease(ctx, original.WorkspaceID, "result-continuation-retention", req)
	if err != nil || !decision.ReleaseAllowed {
		t.Fatalf("consumed forwarded review retained resources forever: %+v %v", decision, err)
	}
}

func TestManagedReturnContinuationPreservesTakeoverAuthority(t *testing.T) {
	ctx := context.Background()
	source, result, original := resultFixture(t, "continuation-takeover-authority")
	finishResultFixture(t, result)
	selected, _ := continuationRun(t, source, "takeover-selected")
	dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, selected.ID, testUserID)
	pat, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, original, testUserID, pat}
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	action := decodeTakeover(t, callTakeover(t, f, takeoverBody(t, f, delegation.KindManagedTask, uuidToString(selected.ID))), http.StatusCreated)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, selected.ID)
	other := dbfx.User(t, "Other takeover caller", fmt.Sprintf("continuation-takeover-%d@example.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, other, "member")
	resumed, token := continuationRun(t, selected, "takeover-resumed")
	dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, resumed.ID, other)
	worker, _ := executableTarget(t, "takeover-continuation-reviewer")
	dispatchContinuation(t, token, result, worker, http.StatusCreated)
	current := currentContinuationReturn(t, original)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: parseUUID(action.TargetReturnID)})
	if err != nil || !link.NotificationTaskID.Valid || link.DeliveryReturnID != current.ID {
		t.Fatalf("takeover result did not follow certified continuation: %+v %v", link, err)
	}
	handling, err := testHandler.Queries.GetAgentTask(ctx, link.NotificationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if handling.OriginatorUserID != parseUUID(testUserID) || handling.AccountableUserID != parseUUID(testUserID) {
		t.Fatalf("forwarding replaced explicit takeover authority with the new caller: originator=%s accountable=%s", uuidToString(handling.OriginatorUserID), uuidToString(handling.AccountableUserID))
	}
	if _, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, handling); err != nil {
		t.Fatalf("preserved takeover authority cannot verify: %v", err)
	}
}

func TestManagedReturnContinuationRejectsUnsafeSources(t *testing.T) {
	for _, kind := range []string{"old-active", "concurrent", "other-issue", "unconfirmed-pin", "same-task", "runtime", "provider"} {
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
			case "runtime":
				other := dbfx.Runtime(t, "changed continuation runtime", testutil.Cols{"provider": "codex"})
				dbfx.Exec(t, `UPDATE agent SET runtime_id=$2 WHERE id=$1`, source.AgentID, other)
				dbfx.Exec(t, `UPDATE agent_task_queue SET runtime_id=$2 WHERE id=$1`, run.ID, other)
			case "provider":
				dbfx.Exec(t, `UPDATE agent_runtime SET provider='claude' WHERE id=$1`, source.RuntimeID)
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
