package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func externalUpstream(t *testing.T, source db.AgentTaskQueue, name string, linked bool) db.IssueDelegationReturn {
	t.Helper()
	agentID, sessionID, _ := externalAgentFixture(t, name, "continuation-"+name)
	id := dbfx.Insert(t, "issue_delegation_return", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": uuidToString(source.IssueID),
		"dispatcher_agent_id": agentID, "return_kind": "external_session", "external_session_id": sessionID,
	})
	if linked {
		if _, err := testHandler.Queries.LinkAgentTaskDelegationReturn(context.Background(), db.LinkAgentTaskDelegationReturnParams{TaskID: source.ID, DelegationReturnID: parseUUID(id), CallerTaskIds: []pgtype.UUID{}}); err != nil {
			t.Fatal(err)
		}
	}
	address, err := testHandler.Queries.GetDelegationReturnByID(context.Background(), parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func requireUpstreamReturns(t *testing.T, turn db.AgentTaskQueue, wanted ...db.IssueDelegationReturn) {
	t.Helper()
	actual := taskReturnIDs(t, uuidToString(turn.ID))
	if len(actual) != len(wanted) {
		t.Fatalf("source continuation carries %d returns, want %d: %+v", len(actual), len(wanted), actual)
	}
	want := make(map[string]bool, len(wanted))
	for _, row := range wanted {
		want[uuidToString(row.ID)] = true
	}
	for _, row := range actual {
		if !want[uuidToString(row.ID)] {
			t.Fatalf("continuation inherited unrelated return %s", uuidToString(row.ID))
		}
	}
}

func completeContinuation(t *testing.T, turn db.AgentTaskQueue, claim AgentTaskResponse, output string) {
	t.Helper()
	address := claim.DelegationResult
	testutil.Call(t, testHandler.CompleteTask, withURLParam(newOwnerDaemonRequest("POST", "/complete", map[string]string{
		"output": output, "session_id": address.SessionID, "work_dir": address.WorkDir,
	}), "taskId", uuidToString(turn.ID))).Want(http.StatusOK)
}

func TestDelegationResultContinuesOnlySourceUpstream(t *testing.T) {
	ctx := context.Background()
	source, review, _ := resultFixture(t, "source-upstream")
	first := externalUpstream(t, source, "first-upstream", true)
	second := externalUpstream(t, source, "second-upstream", true)
	externalUpstream(t, source, "unrelated-upstream", false)
	finishResultFixture(t, review)
	fix, fixClaim := startResultHandlingTurn(t, review)
	requireUpstreamReturns(t, fix, first, second)
	if fixClaim.AcceptedReturns == nil || len(*fixClaim.AcceptedReturns) != 2 {
		t.Fatal("continued run cannot see its original dispatchers")
	}

	token := taskTokenFixture(t, uuidToString(fix.AgentID), uuidToString(fix.ID))
	code, _, body := intentComment(t, token, uuidToString(review.IssueID), fmt.Sprintf("[@Review](mention://agent/%s) inspect the repaired artifact", uuidToString(review.AgentID)), "")
	if code != http.StatusCreated {
		t.Fatalf("second review dispatch: %d %s", code, body)
	}
	secondReviewID := latestTaskIDFor(t, uuidToString(review.IssueID), uuidToString(review.AgentID))
	if secondReviewID == uuidToString(review.ID) {
		t.Fatal("second review did not create a distinct run")
	}
	secondReview, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(secondReviewID))
	if err != nil {
		t.Fatal(err)
	}
	completeContinuation(t, fix, fixClaim, "Repaired artifact submitted for review")
	finishResultFixture(t, secondReview)
	final, finalClaim := startResultHandlingTurn(t, secondReview)
	requireUpstreamReturns(t, final, first, second)
	completeContinuation(t, final, finalClaim, "Reviewed artifact ready for acceptance")
	for range 3 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []db.IssueDelegationReturn{first, second} {
		var count int
		var destination string
		dbfx.QueryRow(t, `SELECT count(*),COALESCE(min(m.target_session_id::text),'')
			FROM agent_task_delegation_return l JOIN agent_mailbox_message m ON m.id=l.notification_delivery_id
			WHERE l.task_id=$1 AND l.delegation_return_id=$2`, final.ID, address.ID).Scan(&count, &destination)
		if count != 1 || destination != uuidToString(address.ExternalSessionID) {
			t.Fatalf("final result did not return once to original dispatcher: count=%d destination=%s", count, destination)
		}
	}
	if _, err := testHandler.TaskService.CompleteTask(service.WithTerminalReport(ctx), final.ID, []byte(`{"output":"Reviewed artifact ready for acceptance"}`), finalClaim.DelegationResult.SessionID, finalClaim.DelegationResult.WorkDir, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var notifications int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_delivery_id IS NOT NULL`, final.ID).Scan(&notifications)
	if notifications != 2 {
		t.Fatalf("duplicate outcome report duplicated notifications: %d", notifications)
	}
}

func TestDelegationResultRecoversLateUpstreamAfterRetry(t *testing.T) {
	ctx := context.Background()
	source, review, _ := resultFixture(t, "late-upstream")
	ancestorID := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{
		"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "failed",
	})
	dbfx.Exec(t, `UPDATE agent_task_queue SET retry_of_task_id=$2 WHERE id=$1`, source.ID, ancestorID)
	ancestor, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(ancestorID))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, review)
	continued, claim := startResultHandlingTurn(t, review)
	requireUpstreamReturns(t, continued)
	completeContinuation(t, continued, claim, "Continuation completed before late relationship committed")
	upstream := externalUpstream(t, ancestor, "late-root", true)
	for range 3 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	requireUpstreamReturns(t, continued, upstream)
	var count int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2 AND notification_delivery_id IS NOT NULL`, continued.ID, upstream.ID).Scan(&count)
	if count != 1 {
		t.Fatalf("late upstream did not receive completed continuation exactly once: %d", count)
	}
}

func TestMutualDelegationResultPropagationTerminates(t *testing.T) {
	ctx := context.Background()
	left, right, _ := resultFixture(t, "mutual-result")
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='mutual-right-daemon' WHERE id=$1`, right.RuntimeID)
	finishResultFixture(t, right)
	right, _ = testHandler.Queries.GetAgentTask(ctx, right.ID)
	reverseID := dbfx.Insert(t, "issue_delegation_return", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": left.IssueID, "dispatcher_agent_id": right.AgentID,
		"return_kind": "managed_task", "source_task_id": right.ID, "runtime_id": right.RuntimeID,
		"provider": "codex", "session_id": right.SessionID.String, "work_dir": right.WorkDir.String,
	})
	if _, err := testHandler.Queries.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{TaskID: left.ID, DelegationReturnID: parseUUID(reverseID), CallerTaskIds: []pgtype.UUID{right.ID}}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		nextLeft, leftClaim := startResultHandlingTurn(t, left)
		nextRight, rightClaim := startResultHandlingTurn(t, right)
		if round == 1 {
			requireUpstreamReturns(t, nextLeft)
			requireUpstreamReturns(t, nextRight)
		}
		completeContinuation(t, nextLeft, leftClaim, "Handled mutual outcome")
		completeContinuation(t, nextRight, rightClaim, "Handled mutual outcome")
		left, right = nextLeft, nextRight
	}
	for range 3 {
		stats, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Materialized != 0 {
			t.Fatalf("result forwarding did not terminate: %+v", stats)
		}
	}
}

func TestResultTakeoverDoesNotInheritRecipientWork(t *testing.T) {
	ctx := context.Background()
	source, result, address := resultFixture(t, "one-off-takeover")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, address, testUserID, token}
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	targetID := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{
		"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running",
		"session_id": "one-off-target", "work_dir": "/work/one-off-target", "resource_control_required": true,
		"session_pin_generation": testutil.Raw("gen_random_uuid()"), "session_pin_confirmed": true,
	})
	target, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(targetID))
	if err != nil {
		t.Fatal(err)
	}
	externalUpstream(t, target, "unrelated-takeover-root", true)
	accepted := decodeTakeover(t, callTakeover(t, f, takeoverBody(t, f, delegation.KindManagedTask, targetID)), http.StatusCreated)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: parseUUID(accepted.TargetReturnID)})
	if err != nil || !link.NotificationTaskID.Valid {
		t.Fatalf("takeover did not materialize: %+v %v", link, err)
	}
	handling, err := testHandler.Queries.GetAgentTask(ctx, link.NotificationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	requireUpstreamReturns(t, handling)
}

func TestDelegationContinuationInheritanceCommitsWithNotification(t *testing.T) {
	ctx := context.Background()
	source, result, address := resultFixture(t, "atomic-continuation")
	upstream := externalUpstream(t, source, "atomic-upstream", true)
	finishResultFixture(t, result)
	name := "reject_continuation_" + strings.ReplaceAll(uuidToString(source.ID), "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN
        IF NEW.task_id <> '%s' AND NEW.delegation_return_id = '%s' THEN
            RAISE EXCEPTION 'inherited return unavailable';
        END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
        CREATE TRIGGER %s BEFORE INSERT ON agent_task_delegation_return FOR EACH ROW EXECUTE FUNCTION %s();`, name, uuidToString(source.ID), uuidToString(upstream.ID), name, name))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, name, name))
	}
	t.Cleanup(drop)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: address.ID})
	if err != nil || link.NotificationTaskID.Valid || link.MaterializedAt.Valid {
		t.Fatalf("failed inheritance published a partial result turn: %+v %v", link, err)
	}
	var tasks int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND context->>'type'='delegation_result'`, source.AgentID).Scan(&tasks)
	if tasks != 0 {
		t.Fatalf("failed transaction left %d orphan result turns", tasks)
	}
	drop()
	dbfx.Exec(t, `UPDATE agent_task_delegation_return SET last_attempt_at=NULL WHERE task_id=$1`, result.ID)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err = testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: address.ID})
	if err != nil || !link.NotificationTaskID.Valid {
		t.Fatalf("recovered inheritance did not materialize: %+v %v", link, err)
	}
	turn, err := testHandler.Queries.GetAgentTask(ctx, link.NotificationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	requireUpstreamReturns(t, turn, upstream)
}

func TestManagedAncestorRetainedAcrossDownstreamReview(t *testing.T) {
	ctx := context.Background()
	ancestor, implementer, upstream := resultFixture(t, "managed-ancestor")
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='middle-review-daemon' WHERE id=$1`, implementer.RuntimeID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),session_id='middle-session',work_dir='/work/middle',resource_control_required=true,session_pin_confirmed=true,session_pin_generation=gen_random_uuid() WHERE id=$1`, implementer.ID)
	implementer, _ = testHandler.Queries.GetAgentTask(ctx, implementer.ID)
	token := taskTokenFixture(t, uuidToString(implementer.AgentID), uuidToString(implementer.ID))
	reviewer, _ := executableTarget(t, "managed-ancestor-reviewer")
	title := fmt.Sprintf("nested review %s", uuidToString(implementer.ID))
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, reviewer), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("nested review: %d %s", w.Code, w.Body.String())
	}
	reviewIssue, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, reviewIssue)
	reviewID := latestTaskIDFor(t, reviewIssue, reviewer)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1`, reviewID)
	dbfx.Exec(t, `UPDATE issue SET status='in_review' WHERE id=$1`, implementer.IssueID)
	if _, err := testHandler.TaskService.CompleteTask(service.WithTerminalReport(ctx), implementer.ID, []byte(`{"output":"Review pending"}`), implementer.SessionID.String, implementer.WorkDir.String, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	completeResultHandlingTurn(t, implementer)
	request := protocol.ResourceReleaseRequest{Kind: "workdir", WorkspaceID: testWorkspaceID, SourceTaskID: uuidToString(ancestor.ID), RuntimeID: uuidToString(ancestor.RuntimeID), Provider: "codex", WorkDir: ancestor.WorkDir.String, ResourcePath: ancestor.WorkDir.String}
	var pendingReview protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", request)).Want(http.StatusOK).JSON(&pendingReview)
	if pendingReview.ReleaseAllowed || pendingReview.Reason != "delegation_not_accepted" {
		t.Fatalf("ancestor resources released while downstream review still runs: %+v", pendingReview)
	}
	review, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(reviewID))
	finishResultFixture(t, review)
	continued, claim := startResultHandlingTurn(t, review)
	requireUpstreamReturns(t, continued, upstream)
	completeContinuation(t, continued, claim, "Reviewed delivery ready")
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, implementer.IssueID)
	var pendingFinal protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", request)).Want(http.StatusOK).JSON(&pendingFinal)
	if pendingFinal.ReleaseAllowed {
		t.Fatalf("ancestor resources released before its final result handling: %+v", pendingFinal)
	}
	completeResultHandlingTurn(t, continued)
	var accepted protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", request)).Want(http.StatusOK).JSON(&accepted)
	if !accepted.ReleaseAllowed {
		t.Fatalf("accepted fully handled work retained indefinitely: %+v", accepted)
	}
}
