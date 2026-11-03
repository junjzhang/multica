package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestDelegationRetryRequiresCurrentExecutionPin(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	agentID, sourceID, _ := managedDispatcherFixture(t, "retry-current-pin")
	source, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local', provider='codex', daemon_id='result-pin-daemon' WHERE id=$1`, source.RuntimeID)
	_, err = testHandler.TaskService.FailTask(ctx, source.ID, "temporary network", source.SessionID.String, source.WorkDir.String, "", "agent_error.provider_network", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := testHandler.Queries.GetAutomaticRetryChild(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimTaskForRuntimeGuardWithCapabilities(t, uuidToString(source.RuntimeID), "result-pin-daemon", protocol.DaemonCapabilityDelegationResultsV1)
	retry, err = testHandler.Queries.GetAgentTask(ctx, retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, testHandler.StartTask, startClaimRequest(uuidToString(retry.ID), uuidToString(retry.RuntimeID), retry.DispatchedAt.Time)).Want(http.StatusOK)
	token := taskTokenFixture(t, agentID, uuidToString(retry.ID))
	target, _ := executableTarget(t, "retry pin target")
	bumpIssueCounter(t)
	title := fmt.Sprintf("retry unconfirmed pin %d", time.Now().UnixNano())
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("unconfirmed retry was allowed: status=%d (inherited session=%q)", w.Code, retry.SessionID.String)
	}
	if _, exists := issueIDByTitle(t, title); exists {
		t.Fatal("rejected admission created issue")
	}
	pinReq := withURLParam(newOwnerDaemonRequest("POST", "/pin-session", map[string]string{"session_id": "actual-retry-B", "work_dir": "/work/retry-B", "generation": uuidToString(retry.SessionPinGeneration)}), "taskId", uuidToString(retry.ID))
	testutil.Call(t, testHandler.PinTaskSession, pinReq).Want(http.StatusNoContent)
	w = serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("confirmed retry rejected: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	accepted, ok := acceptedReturn(t, issueID)
	if !ok || accepted.SessionID.String != "actual-retry-B" || accepted.WorkDir.String != "/work/retry-B" {
		t.Fatal("return did not freeze actual retry B")
	}
}

func TestDelegationResultMaterializesFrozenOriginalTurn(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	_, sourceID, token := managedDispatcherFixture(t, "result-frozen")
	source, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='result-frozen-daemon' WHERE id=$1`, source.RuntimeID)
	target, _ := executableTarget(t, "result-frozen-target")
	title := fmt.Sprintf("result frozen %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	accepted, _ := acceptedReturn(t, issueID)
	resultID := latestTaskIDFor(t, issueID, target)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1`, resultID)
	_, err = testHandler.TaskService.CompleteTask(service.WithTerminalReport(ctx), parseUUID(resultID), []byte(`{"output":"delivered artifact"}`), "result-session", "/work/result", "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Source has ended and moved; neither fact changes the accepted original tuple.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),session_id='new-session',work_dir='/work/new' WHERE id=$1`, source.ID)
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := testHandler.Queries.ListTaskResultNotifications(ctx, db.ListTaskResultNotificationsParams{TaskID: parseUUID(resultID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].NotificationTaskID.Valid || rows[0].NotificationError.Valid {
		t.Fatalf("notification not materialized: %+v", rows)
	}
	turn, err := testHandler.Queries.GetAgentTask(ctx, rows[0].NotificationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.IssueID != source.IssueID || turn.SessionID != accepted.SessionID || turn.WorkDir != accepted.WorkDir {
		t.Fatal("frozen source address drifted")
	}
	if len(taskReturnIDs(t, uuidToString(turn.ID))) != 0 {
		t.Fatal("result turn inherited executor return links")
	}
	verified, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, turn)
	if err != nil || verified == nil {
		t.Fatalf("verify: %v", err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1`, turn.ID)
	turn, _ = testHandler.Queries.GetAgentTask(ctx, turn.ID)
	rt, _ := testHandler.Queries.GetAgentRuntime(ctx, turn.RuntimeID)
	req := newOwnerDaemonRequest("POST", "/claim", nil)
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	resp, ids, _, _, _, failure := testHandler.buildClaimedTaskResponse(req, &turn, rt, uuidToString(rt.ID), testWorkspaceID)
	if failure != nil || !resp.RequireResume || resp.DelegationResult == nil || resp.PriorSessionID != accepted.SessionID.String || len(ids) != 0 {
		t.Fatalf("strict claim: failure=%+v resume=%v session=%q ids=%v", failure, resp.RequireResume, resp.PriorSessionID, ids)
	}
	testutil.Call(t, testHandler.PinTaskSession, withURLParam(newOwnerDaemonRequest("POST", "/pin-session", map[string]string{"session_id": "wrong-new-session"}), "taskId", uuidToString(turn.ID))).Want(http.StatusConflict)
}

func resultFixture(t *testing.T, name string) (source db.AgentTaskQueue, result db.AgentTaskQueue, address db.IssueDelegationReturn) {
	t.Helper()
	ctx := context.Background()
	_, sourceID, token := managedDispatcherFixture(t, name)
	source, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id=$2 WHERE id=$1`, source.RuntimeID, "result-"+name)
	target, _ := executableTarget(t, "target-"+name)
	title := fmt.Sprintf("result %s %d", name, time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	address, _ = acceptedReturn(t, issueID)
	result, err = testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, issueID, target)))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, source.ID)
	return source, result, address
}

func finishResultFixture(t *testing.T, result db.AgentTaskQueue) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1`, result.ID)
	if _, err := testHandler.TaskService.CompleteTask(service.WithTerminalReport(context.Background()), result.ID, []byte(`{"output":"result artifact"}`), "child-session", "/work/child", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestDelegationResultRecoveryIsAtomicAndConcurrent(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, _ := resultFixture(t, "atomic-result")
	finishResultFixture(t, result)
	fn := "fail_result_pointer_" + strings.ReplaceAll(uuidToString(result.ID), "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN IF NEW.task_id='%s' AND NEW.notification_task_id IS NOT NULL THEN RAISE EXCEPTION 'result pointer failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql; CREATE TRIGGER %s BEFORE UPDATE ON agent_task_delegation_return FOR EACH ROW EXECUTE FUNCTION %s();`, fn, uuidToString(result.ID), fn, fn))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND context->>'type'='delegation_result'`, source.AgentID); n != 0 {
		t.Fatalf("partial materialization left %d tasks", n)
	}
	drop()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND context->>'type'='delegation_result'`, source.AgentID); n != 1 {
		t.Fatalf("recovery created %d tasks", n)
	}
}

func TestDelegationResultRetryInheritanceAndLateLink(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	for _, path := range []string{"terminal-transaction", "recovery"} {
		t.Run(path, func(t *testing.T) {
			_, result, address := resultFixture(t, "retry-"+path)
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),max_attempts=3 WHERE id=$1`, result.ID)
			if path == "terminal-transaction" {
				_, err := testHandler.TaskService.FailTask(service.WithTerminalReport(ctx), result.ID, "network", "", "", "", "agent_error.provider_network", false, "", "")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',failure_reason='agent_error.provider_network',completed_at=now() WHERE id=$1`, result.ID)
				failed, err := testHandler.Queries.GetAgentTask(ctx, result.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := testHandler.TaskService.MaybeRetryFailedTask(ctx, failed); err != nil {
					t.Fatal(err)
				}
			}
			child, err := testHandler.Queries.GetAutomaticRetryChild(ctx, result.ID)
			if err != nil {
				t.Fatal(err)
			}
			requireTaskReturns(t, uuidToString(child.ID), address)
			if child.ResourceControlRequired {
				t.Fatal("retry inherited claim agreement without being claimed")
			}
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Fatal(err)
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND (notification_task_id IS NOT NULL OR notification_delivery_id IS NOT NULL)`, result.ID); n != 0 {
				t.Fatal("intermediate failure notified")
			}
			finishResultFixture(t, child)
			// Re-link the late-discovered parent relation after the retry has ended.
			dbfx.Exec(t, `DELETE FROM agent_task_delegation_return WHERE task_id=$1`, child.ID)
			if _, err := testHandler.Queries.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{TaskID: result.ID, DelegationReturnID: address.ID}); err != nil {
				t.Fatal(err)
			}
			requireTaskReturns(t, uuidToString(child.ID), address)
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Fatal(err)
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_task_id IS NOT NULL`, child.ID); n != 1 {
				t.Fatal("late final outcome was not notified")
			}
		})
	}
}

func TestDelegationResourceRetentionAndReleaseReceipt(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, _ := resultFixture(t, "resource-release")
	finishResultFixture(t, result)
	req := protocol.ResourceReleaseRequest{Kind: "workdir", SourceTaskID: uuidToString(source.ID), WorkspaceID: testWorkspaceID, ResourcePath: "/work/dispatcher-resource-release", WorkDir: source.WorkDir.String}
	check := func() protocol.ResourceReleaseDecision {
		t.Helper()
		var out protocol.ResourceReleaseDecision
		testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", req)).Want(200).JSON(&out)
		return out
	}
	if out := check(); out.ReleaseAllowed || out.Reason != "delegation_not_accepted" {
		t.Fatalf("result completion released unaccepted work: %+v", out)
	}
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, result.IssueID)
	if out := check(); out.ReleaseAllowed {
		t.Fatalf("queued result counted as received: %+v", out)
	}
	var turnID string
	dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, result.ID).Scan(&turnID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',failure_reason=$2,completed_at=now(),terminal_reported_at=now() WHERE id=$1`, turnID, protocol.DelegationResultInputUnknown)
	if out := check(); out.ReleaseAllowed {
		t.Fatal("unknown result delivery released frozen resources")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, turnID)
	if out := check(); !out.ReleaseAllowed || !out.AdmissionClosed {
		t.Fatalf("received and accepted still retained: %+v", out)
	}
	receipt := protocol.ResourceReleaseReceipt{ResourceReleaseRequest: req, DurableWorkDir: "/repo/durable"}
	for range 2 {
		var out protocol.ResourceReleaseAcknowledgement
		testutil.Call(t, testHandler.RecordResourcesReleased, withURLParam(newOwnerDaemonRequest("POST", "/resources-released", receipt), "taskId", uuidToString(source.ID))).Want(200).JSON(&out)
		if !out.Recorded || out.DurableWorkDir != "/repo/durable" {
			t.Fatalf("receipt: %+v", out)
		}
	}
	var rawResult string
	dbfx.QueryRow(t, `SELECT COALESCE(result::text,'null') FROM agent_task_queue WHERE id=$1`, source.ID).Scan(&rawResult)
	if rawResult != "null" {
		t.Fatal("location refinement rewrote outcome")
	}
}

func TestDelegationResultPendingRetryDecisionDoesNotNotify(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	_, result, address := resultFixture(t, "retry-slot")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),max_attempts=3 WHERE id=$1`, result.ID)
	blocker := dbfx.Task(t, uuidToString(result.AgentID), testutil.Cols{"issue_id": uuidToString(result.IssueID), "runtime_id": uuidToString(result.RuntimeID), "status": "queued"})
	if _, err := testHandler.TaskService.FailTask(service.WithTerminalReport(ctx), result.ID, "network", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := testHandler.Queries.GetAgentTask(ctx, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.DelegationOutcomeFinal {
		t.Fatal("occupied retry slot was treated as final failure")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_task_id IS NOT NULL`, result.ID); n != 0 {
		t.Fatal("retry decision window published failure")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, blocker)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	child, err := testHandler.Queries.GetAutomaticRetryChild(ctx, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireTaskReturns(t, uuidToString(child.ID), address)
}

func TestDelegationResultExternalUsesAcceptedSession(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	dispatcher, sessionID, _ := externalAgentFixture(t, "result external", "result-external-daemon")
	var ownerID, hostSession string
	dbfx.QueryRow(t, `SELECT owner_user_id,session_id FROM external_agent_session WHERE id=$1`, sessionID).Scan(&ownerID, &hostSession)
	token := externalTokenFixture(t, dispatcher, ownerID, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "external-result-target")
	title := fmt.Sprintf("external result %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, hostSession) })
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, issueID, target)))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, result)
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	var deliveries int
	var targetSession, sourceType string
	dbfx.QueryRow(t, `SELECT count(*),min(m.target_session_id::text),min(m.source_type) FROM agent_mailbox_message m JOIN agent_task_delegation_return l ON l.notification_delivery_id=m.id WHERE l.task_id=$1`, result.ID).Scan(&deliveries, &targetSession, &sourceType)
	if deliveries != 1 || targetSession != sessionID || sourceType != "delegation_result" {
		t.Fatalf("generic notification count=%d target=%q type=%q", deliveries, targetSession, sourceType)
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	host := &resultWorkerHost{received: make(chan delivery.Delivery, 2), session: hostSession}
	client := &resultWorkerClient{queue: delivery.NewQueue(testHandler.Queries, testPool), daemon: "result-external-daemon", owner: ownerID, done: cancel}
	_ = delivery.NewWorker(client, host, func() []string { return []string{testWorkspaceID} }, client.daemon, nil).Run(runCtx)
	select {
	case delivered := <-host.received:
		var payload map[string]string
		if err := json.Unmarshal(delivered.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if delivered.Kind != "delegation_result" || delivered.Refs.IssueID != issueID || payload["result_task_id"] != uuidToString(result.ID) || payload["outcome"] != "completed" {
			t.Fatalf("host did not receive exact outcome references: %+v", payload)
		}
		t.Logf("fake host received kind=%s result_task_id=%s issue_id=%s outcome=%s", delivered.Kind, payload["result_task_id"], payload["issue_id"], payload["outcome"])
	default:
		t.Fatal("real delivery claim/worker never reached fake host")
	}
	var status, receipt string
	dbfx.QueryRow(t, `SELECT m.delivery_status,COALESCE(m.host_receipt,'') FROM agent_mailbox_message m JOIN agent_task_delegation_return l ON l.notification_delivery_id=m.id WHERE l.task_id=$1`, result.ID).Scan(&status, &receipt)
	if status != "delivered" || receipt == "" {
		t.Fatalf("host receipt not persisted: %s/%q", status, receipt)
	}

}

func TestDelegationResultRefusesUnavailableOriginalTargets(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	for _, kind := range []string{"cloud", "archived", "source-scope", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			source, result, _ := resultFixture(t, "refuse-"+kind)
			finishResultFixture(t, result)
			switch kind {
			case "cloud":
				dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='cloud' WHERE id=$1`, source.RuntimeID)
			case "archived":
				dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, source.AgentID)
			case "source-scope":
				dbfx.Exec(t, `DELETE FROM issue WHERE id=$1`, source.IssueID)
			case "cancelled":
				dbfx.Exec(t, `UPDATE issue SET status='cancelled' WHERE id=$1`, result.IssueID)
			}
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Fatal(err)
			}
			rows, err := testHandler.Queries.ListTaskResultNotifications(ctx, db.ListTaskResultNotificationsParams{TaskID: result.ID, WorkspaceID: parseUUID(testWorkspaceID)})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || !rows[0].NotificationError.Valid || rows[0].NotificationTaskID.Valid {
				t.Fatalf("refusal missing: %+v", rows)
			}
			persisted, err := testHandler.Queries.GetAgentTask(ctx, result.ID)
			if err != nil || len(persisted.Result) == 0 {
				t.Fatal("unavailable recipient lost authoritative result")
			}
		})
	}
}

func TestDelegationClaimResourceAgreementIsAtomic(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	id, runtimeID, _ := startClaimFixture(t, "dispatched")
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := testHandler.Queries.GetAgentRuntime(ctx, parseUUID(runtimeID))
	if err != nil {
		t.Fatal(err)
	}
	token := db.CreateTaskTokenParams{TokenHash: "resource-claim-" + id, TaskID: task.ID, AgentID: task.AgentID, WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(testUserID), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
	response := AgentTaskResponse{ResourceControlRequired: true}
	_, _, err = testHandler.finalizeClaimDelivery(ctx, &task, rt, runtimeID, testWorkspaceID, &response, token, []pgtype.UUID{parseUUID("00000000-0000-0000-0000-000000000099")}, true, nil)
	if err == nil {
		t.Fatal("invalid receipt unexpectedly committed")
	}
	after, _ := testHandler.Queries.GetAgentTask(ctx, task.ID)
	if after.ResourceControlRequired || countValue(t, `SELECT count(*) FROM task_token WHERE token_hash=$1`, token.TokenHash) != 0 {
		t.Fatal("claim rollback leaked protocol agreement or token")
	}
	// A pre-upgrade queued task acquires the agreement only when delivered to v1.
	_, _, err = testHandler.finalizeClaimDelivery(ctx, &task, rt, runtimeID, testWorkspaceID, &response, token, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = testHandler.Queries.GetAgentTask(ctx, task.ID)
	if !after.ResourceControlRequired || after.SessionPinConfirmed {
		t.Fatal("new claim did not require a fresh confirmed provider pin")
	}
	if countValue(t, `SELECT count(*) FROM task_token WHERE token_hash=$1`, token.TokenHash) != 1 {
		t.Fatal("agreement did not commit with token")
	}
}

func TestDelegationResourceLegacyAndTerminalObservation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	for _, kind := range []string{"legacy", "complete", "fail", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			rt := dbfx.Runtime(t, "resource receipt "+kind, testutil.Cols{"runtime_mode": "local", "provider": "codex", "daemon_id": "receipt-" + kind})
			ag := dbfx.Agent(t, "resource receipt "+kind, rt)
			issue := dbfx.Issue(t, "resource receipt "+kind)
			id := dbfx.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issue, "status": "running", "resource_control_required": kind != "legacy", "work_dir": "/receipt/" + kind})
			if kind == "legacy" {
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, id)
			} else {
				// A control transition alone is not proof that the executor has stopped.
				if _, err := testHandler.TaskService.CancelTask(ctx, parseUUID(id)); err != nil {
					t.Fatal(err)
				}
			}
			req := protocol.ResourceReleaseRequest{Kind: "task", SourceTaskID: id}
			check := func() protocol.ResourceReleaseDecision {
				var out protocol.ResourceReleaseDecision
				testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", req)).Want(200).JSON(&out)
				return out
			}
			before := check()
			if kind == "legacy" {
				if !before.ReleaseAllowed || before.ControlRequired {
					t.Fatalf("legacy owned resource lost TTL compatibility: %+v", before)
				}
				return
			}
			if before.ReleaseAllowed || before.Reason != "execution_exit_unknown" {
				t.Fatalf("control terminal released live executor: %+v", before)
			}
			var callback http.HandlerFunc
			body := map[string]string{}
			switch kind {
			case "complete":
				callback = testHandler.CompleteTask
				body["output"] = "late completion"
			case "fail":
				callback = testHandler.FailTask
				body["error"] = "late failure"
			case "cancel":
				callback = testHandler.AckTaskCancelled
			}
			var stamp pgtype.Timestamptz
			for i := range 2 {
				testutil.Call(t, callback, withURLParam(newOwnerDaemonRequest("POST", "/terminal", body), "taskId", id)).Want(200)
				after, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(id))
				if !after.TerminalReportedAt.Valid {
					t.Fatal("actual terminal callback did not record observation")
				}
				if i == 0 {
					stamp = after.TerminalReportedAt
				} else if !stamp.Time.Equal(after.TerminalReportedAt.Time) {
					t.Fatal("terminal replay changed original observation")
				}
			}
			if out := check(); !out.ReleaseAllowed {
				t.Fatalf("confirmed terminal remained protected: %+v", out)
			}
		})
	}
}

func TestDelegationResourceSharedDirectoryAndProviderStore(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	source, _, _ := resultFixture(t, "shared-resource")
	rt := dbfx.Runtime(t, "other same-daemon runtime", testutil.Cols{"runtime_mode": "local", "provider": "hermes", "daemon_id": "result-shared-resource"})
	other := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"runtime_id": rt, "issue_id": uuidToString(source.IssueID), "status": "running", "session_id": "another-session", "work_dir": "/shared-root/other-session"})
	storeUser := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"runtime_id": uuidToString(source.RuntimeID), "issue_id": uuidToString(source.IssueID), "status": "running", "session_id": "another-codex-session", "work_dir": "/store/user"})
	for _, req := range []protocol.ResourceReleaseRequest{
		{Kind: "workdir", SourceTaskID: uuidToString(source.ID), ResourcePath: "/shared-root"},
		{Kind: "provider_store", RuntimeID: uuidToString(source.RuntimeID), Provider: "codex", AgentID: uuidToString(source.AgentID), ScopeType: "issue", ScopeID: uuidToString(source.IssueID)},
	} {
		var out protocol.ResourceReleaseDecision
		testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", req)).Want(200).JSON(&out)
		if out.ReleaseAllowed {
			t.Fatal("shared resource released while another runtime/session used it")
		}
		found := false
		for _, a := range out.ProtectedResources {
			found = found || a.SourceTaskID == other || a.SourceTaskID == storeUser
		}
		if !found {
			t.Fatalf("other runtime was not considered: %+v", out)
		}
	}
}

func TestDelegationSessionPinGenerationFencesLateAttempts(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	ag, sourceID, token := managedDispatcherFixture(t, "pin-generation")
	source, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := executableTarget(t, "pin-generation-target")
	generationA := uuidToString(source.SessionPinGeneration)
	generationB := uuidToString(dbid.NewV7())
	reset := protocol.SessionPinRequest{Generation: generationA, NextGeneration: generationB}
	call := func(body protocol.SessionPinRequest, code int) {
		t.Helper()
		testutil.Call(t, testHandler.PinTaskSession, withURLParam(newOwnerDaemonRequest("POST", "/session", body), "taskId", sourceID)).Want(code)
	}
	call(reset, 200)
	call(protocol.SessionPinRequest{Generation: generationA, SessionID: "late-A", WorkDir: "/work/late-A"}, 409)
	after, _ := testHandler.Queries.GetAgentTask(ctx, source.ID)
	if after.SessionPinConfirmed || after.SessionPinGeneration == source.SessionPinGeneration || after.SessionID != source.SessionID {
		t.Fatal("stale pin changed current proof or resume hint")
	}
	bumpIssueCounter(t)
	title := fmt.Sprintf("pin CAS admission %d", time.Now().UnixNano())
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), "POST", "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != 409 {
		t.Fatalf("fresh unconfirmed attempt accepted: %d", w.Code)
	}
	call(protocol.SessionPinRequest{Generation: generationB, SessionID: "actual-B", WorkDir: "/work/actual-B"}, 204)
	call(reset, 200) // A lost reset response must not erase B's now-confirmed pin.
	after, _ = testHandler.Queries.GetAgentTask(ctx, source.ID)
	if !after.SessionPinConfirmed || after.SessionID.String != "actual-B" {
		t.Fatal("reset replay erased new confirmed pin")
	}
	w = serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), "POST", "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != 201 {
		t.Fatalf("confirmed B refused: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	accepted, _ := acceptedReturn(t, issueID)
	call(protocol.SessionPinRequest{Generation: generationB, NextGeneration: uuidToString(dbid.NewV7())}, 200)
	stored, _ := acceptedReturn(t, issueID)
	if stored != accepted {
		t.Fatal("fresh invalidation mutated old accepted snapshot")
	}
	if uuidToString(stored.DispatcherAgentID) != ag {
		t.Fatal("wrong accepted dispatcher")
	}
}

func TestDelegationResourceWorkdirReturnsAliasCandidates(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	source, _, _ := resultFixture(t, "alias-candidates")
	req := protocol.ResourceReleaseRequest{Kind: "workdir", SourceTaskID: uuidToString(source.ID), ResourcePath: "/physically-aliased-path"}
	var out protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", req)).Want(200).JSON(&out)
	found := false
	for _, a := range out.ProtectedResources {
		found = found || a.WorkDir == source.WorkDir.String
	}
	if !found {
		t.Fatal("string-only filter hid a protected physical alias candidate")
	}
}

func TestDelegationNotificationProjectionDistinguishesUnknownAndEmpty(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	id, _, _ := startClaimFixture(t, "completed")
	task, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(id))
	bare := taskToResponse(task, testWorkspaceID)
	raw, _ := json.Marshal(bare)
	if bytes.Contains(raw, []byte(`"result_notifications"`)) {
		t.Fatal("unhydrated read claimed notification knowledge")
	}
	rows := []AgentTaskResponse{bare}
	if err := testHandler.hydrateResultNotifications(ctx, parseUUID(testWorkspaceID), task.IssueID, rows); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(rows[0])
	if !bytes.Contains(raw, []byte(`"result_notifications":[]`)) {
		t.Fatal("empty hydrated result was omitted")
	}
}

func TestDelegationResultStartedCancellationRemainsUnknown(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, _ := resultFixture(t, "cancelled-return")
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var turnID string
	dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, result.ID).Scan(&turnID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),resource_control_required=true WHERE id=$1`, turnID)
	if _, err := testHandler.TaskService.CancelTask(ctx, parseUUID(turnID)); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, testHandler.AckTaskCancelled, withURLParam(newOwnerDaemonRequest("POST", "/cancel-ack", map[string]string{"error_message": "Cancelled after provider input; receipt unknown", "failure_reason": protocol.DelegationResultInputUnknown}), "taskId", turnID)).Want(200)
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, result.IssueID)
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	cancelled, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(turnID))
	if cancelled.Status != "cancelled" || !cancelled.TerminalReportedAt.Valid {
		t.Fatal("cancel acknowledgement changed control fact or lost exit receipt")
	}
	var currentID string
	dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, result.ID).Scan(&currentID)
	if currentID != turnID {
		t.Fatal("unknown cancelled input was resent")
	}
	rows := []AgentTaskResponse{taskToResponse(result, testWorkspaceID)}
	if err := testHandler.hydrateResultNotifications(ctx, parseUUID(testWorkspaceID), result.IssueID, rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ResultNotifications == nil || len(*rows[0].ResultNotifications) != 1 || (*rows[0].ResultNotifications)[0].Status != "unknown" {
		t.Fatal("cancel acknowledgement was reported as result receipt")
	}
	var release protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", protocol.ResourceReleaseRequest{Kind: "task", SourceTaskID: uuidToString(source.ID)})).Want(200).JSON(&release)
	if release.ReleaseAllowed || release.Reason != "result_delivery_unknown" {
		t.Fatalf("cancel acknowledgement released unknown input: %+v", release)
	}
}

type resultWorkerClient struct {
	queue         *delivery.Queue
	daemon, owner string
	done          context.CancelFunc
}

func (c *resultWorkerClient) ClaimDeliveries(ctx context.Context, workspace string, limit int) ([]delivery.Claim, error) {
	return c.queue.Claim(ctx, delivery.ClaimRequest{Scope: delivery.Scope{WorkspaceID: workspace}, DaemonID: c.daemon, OwnerUserID: c.owner, Limit: limit})
}
func (c *resultWorkerClient) RecordDeliveryOutcome(ctx context.Context, workspace string, request delivery.OutcomeRequest) (delivery.Delivery, error) {
	request.Scope = delivery.Scope{WorkspaceID: workspace}
	row, err := c.queue.RecordOutcome(ctx, request)
	if err == nil {
		c.done()
	}
	return row, err
}

type resultWorkerHost struct {
	received chan delivery.Delivery
	session  string
}

func (h *resultWorkerHost) DeliverIfIdle(_ context.Context, target delivery.HostTarget, row delivery.Delivery) (string, error) {
	if target.SessionID != h.session {
		return "", fmt.Errorf("wrong frozen host session")
	}
	h.received <- row
	return "fake-host:" + row.ID, nil
}
func (h *resultWorkerHost) LookupReceipt(context.Context, delivery.HostTarget, string) (delivery.Receipt, error) {
	return delivery.Receipt{}, fmt.Errorf("unexpected receipt lookup")
}

func TestDelegationResultPreservesOriginalUnscopedTask(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	_, sourceID, token := managedDispatcherFixture(t, "unscoped-result")
	source, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='unscoped-result-daemon' WHERE id=$1`, source.RuntimeID)
	quickContext, _ := json.Marshal(service.QuickCreateContext{Type: service.QuickCreateContextType, WorkspaceID: testWorkspaceID, RequesterID: testUserID, Prompt: "Create delegated work"})
	dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=NULL,context=$2::jsonb WHERE id=$1`, source.ID, quickContext)
	target, _ := executableTarget(t, "unscoped-target")
	title := fmt.Sprintf("unscoped result %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	body := createIssueBody(title, target)
	body["origin_type"], body["origin_id"] = "quick_create", sourceID
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), "POST", "/api/issues", body, nil, nil)
	if w.Code != 201 {
		t.Fatalf("unscoped admission: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, issueID, target)))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, result)
	testutil.Call(t, testHandler.CompleteTask, withURLParam(newOwnerDaemonRequest("POST", "/complete", map[string]string{"output": "Created delegated issue", "session_id": source.SessionID.String, "work_dir": source.WorkDir.String}), "taskId", sourceID)).Want(200)
	linkedSource, _ := testHandler.Queries.GetAgentTask(ctx, source.ID)
	if linkedSource.IssueID != result.IssueID {
		t.Fatal("quick-create completion did not exercise LinkTaskToIssue")
	}
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var turnID string
	dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, result.ID).Scan(&turnID)
	turn, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(turnID))
	if err != nil {
		t.Fatal(err)
	}
	if turn.IssueID.Valid || turn.ChatSessionID.Valid || turn.AutopilotRunID.Valid {
		t.Fatal("unscoped source was redirected into a replacement scope")
	}
	proof, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, turn)
	if err != nil || proof == nil || proof.SourceTaskID != sourceID || proof.ScopeTaskID != sourceID {
		t.Fatalf("original task scope verification: %v", err)
	}
	// The result handler may itself accept real child work. Its provenance is R,
	// while its physical namespace remains the original task S.
	req := withURLParam(newOwnerDaemonRequest("POST", "/claim", nil), "runtimeId", uuidToString(source.RuntimeID))
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	var claim struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(200).JSON(&claim)
	if claim.Task == nil || claim.Task.ID != turnID {
		t.Fatal("first typed turn was not claimed")
	}
	turn, _ = testHandler.Queries.GetAgentTask(ctx, turn.ID)
	testutil.Call(t, testHandler.StartTask, startClaimRequest(turnID, uuidToString(turn.RuntimeID), turn.DispatchedAt.Time)).Want(200)
	testutil.Call(t, testHandler.PinTaskSession, withURLParam(newOwnerDaemonRequest("POST", "/session", protocol.SessionPinRequest{SessionID: proof.SessionID, WorkDir: proof.WorkDir, Generation: claim.Task.SessionPinGeneration}), "taskId", turnID)).Want(204)
	nextTitle := title + " next"
	bumpIssueCounter(t)
	w = serveAuthenticated(t, claim.Task.AuthToken, http.HandlerFunc(testHandler.CreateIssue), "POST", "/api/issues", createIssueBody(nextTitle, target), nil, nil)
	if w.Code != 201 {
		t.Fatalf("typed handler new delegation: %d %s", w.Code, w.Body.String())
	}
	nextIssue, _ := issueIDByTitle(t, nextTitle)
	cleanupDelegatedIssue(t, nextIssue)
	nextAddress, ok := acceptedReturn(t, nextIssue)
	if !ok || uuidToString(nextAddress.ScopeTaskID) != sourceID {
		t.Fatal("new delegation did not freeze its original task scope")
	}

	nextResult, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, nextIssue, target)))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, nextResult)
	if _, err := testHandler.TaskService.CompleteTask(service.WithTerminalReport(ctx), turn.ID, []byte(`{"output":"handled prior result"}`), proof.SessionID, proof.WorkDir, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	// The newly accepted child owns its scope fact, independently of the old
	// notification relation that led to R.
	dbfx.Exec(t, `DELETE FROM agent_task_delegation_return WHERE notification_task_id=$1`, turn.ID)
	dbfx.Exec(t, `DELETE FROM issue_delegation_return WHERE id=$1`, parseUUID(proof.DelegationReturnID))
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, result.IssueID)
	// The old notification is gone. Only the new child's frozen scope keeps S.
	testutil.Call(t, testHandler.DeleteIssue, withURLParam(newRequest("DELETE", "/api/issues/"+issueID, nil), "id", issueID)).Want(http.StatusConflict)
	if _, err := testHandler.Queries.GetAgentTask(ctx, source.ID); err != nil {
		t.Fatal("new dependency lost its original scope task")
	}

	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var secondTurnID string
	dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, nextResult.ID).Scan(&secondTurnID)
	secondTurn, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(secondTurnID))
	if err != nil {
		t.Fatal(err)
	}
	nextProof, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, secondTurn)
	if err != nil || nextProof == nil || nextProof.ScopeTaskID != sourceID || nextProof.SourceTaskID != turnID {
		t.Fatalf("typed delegation changed original physical task scope: proof=%+v err=%v", nextProof, err)
	}

	// Actual loss of S is a visible refusal, never a replacement issue/root.
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id=$1`, source.ID)
	var refused struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req.Clone(req.Context())).Want(200).JSON(&refused)
	if refused.Task != nil {
		t.Fatal("deleted original scope still produced an executable claim")
	}
	lost, _ := testHandler.Queries.GetAgentTask(ctx, secondTurn.ID)
	if lost.Status != "failed" || lost.FailureReason.String != protocol.DelegationResultUnavailable {
		t.Fatalf("missing scope not visible: %s/%s", lost.Status, lost.FailureReason.String)
	}

}

func TestDelegationResultNotifiesOnlyActualCarriedRecipients(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, first := resultFixture(t, "multi-recipient")
	if _, err := testHandler.TaskService.CancelTask(ctx, result.ID); err != nil {
		t.Fatal(err)
	}
	firstRun, firstToken := extraManagedRun(t, uuidToString(source.AgentID), first.SessionID.String, first.WorkDir.String)
	rootComment := dbfx.Comment(t, uuidToString(result.IssueID), "One shared dispatch thread")
	code, _, body := intentComment(t, firstToken, uuidToString(result.IssueID), fmt.Sprintf("[@Executor](mention://agent/%s) first accepted input", uuidToString(result.AgentID)), rootComment)
	if code != 201 {
		t.Fatalf("first actual recipient: %d %s", code, body)
	}
	result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, uuidToString(result.IssueID), uuidToString(result.AgentID))))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, firstRun)

	secondAgent, secondID, secondToken := managedDispatcherFixture(t, "multi-recipient-second")
	second, _ := testHandler.Queries.GetAgentTask(ctx, parseUUID(secondID))
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='multi-second-daemon' WHERE id=$1`, second.RuntimeID)
	code, _, body = intentComment(t, secondToken, uuidToString(result.IssueID), fmt.Sprintf("[@Executor](mention://agent/%s) additional accepted input", uuidToString(result.AgentID)), rootComment)
	if code != 201 {
		t.Fatalf("second actual recipient: %d %s", code, body)
	}
	carried := taskReturnIDs(t, uuidToString(result.ID))
	if len(carried) != 2 {
		t.Fatalf("actual batch has %d accepted recipients", len(carried))
	}
	thirdAgent, thirdID, _ := managedDispatcherFixture(t, "multi-recipient-unrelated")
	address, err := delegation.ResolveManaged(ctx, testHandler.Queries, testWorkspaceID, thirdAgent, thirdID)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := delegation.Ensure(ctx, testHandler.Queries, testWorkspaceID, uuidToString(result.IssueID), address, testUserID)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, second.ID)
	finishResultFixture(t, result)
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := testHandler.Queries.ListTaskResultNotifications(ctx, db.ListTaskResultNotificationsParams{TaskID: result.ID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		if !row.NotificationTaskID.Valid || row.DelegationReturnID == unrelated.ID {
			t.Fatal("missing actual notification or notified an uncarried binding")
		}
		got[uuidToString(row.DispatcherAgentID)] = true
	}
	if len(rows) != 2 || !got[uuidToString(source.AgentID)] || !got[secondAgent] {
		t.Fatalf("actual recipient set: %v, first=%s", got, uuidToString(first.ID))
	}
}
