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
				dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=NULL WHERE id=$1`, source.ID)
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
	_, result, _ := resultFixture(t, "cancelled-return")
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
