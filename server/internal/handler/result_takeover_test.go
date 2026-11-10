package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

type takeoverFixture struct {
	result       db.AgentTaskQueue
	address      db.IssueDelegationReturn
	owner, token string
}

func externalTakeoverFixture(t *testing.T) takeoverFixture {
	t.Helper()
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	dispatcher, session, owner := externalAgentFixture(t, "takeover", "takeover-host")
	agentToken := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	executor, _ := executableTarget(t, "takeover executor")
	title := fmt.Sprintf("takeover result %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, agentToken, http.HandlerFunc(testHandler.CreateIssue), "POST", "/issues", createIssueBody(title, executor), nil, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, session) })
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	issue, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issue)
	address, _ := acceptedReturn(t, issue)
	result, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(latestTaskIDFor(t, issue, executor)))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET delivery_status='unknown',last_error='host receipt unknown' WHERE id=(SELECT notification_delivery_id FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2)`, result.ID, address.ID)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	return takeoverFixture{result, address, owner, token}
}

func takeoverExternalSession(t *testing.T, f takeoverFixture, name string) string {
	t.Helper()
	return dbfx.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": testWorkspaceID, "owner_user_id": f.owner, "agent_id": f.address.DispatcherAgentID, "provider": "paseo", "host_id": "host-" + name, "session_id": "session-" + name, "display_name": name, "daemon_id": "takeover-host"})
}

func takeoverBody(t *testing.T, f takeoverFixture, kind delegation.Kind, selector string) resultTakeoverRequest {
	t.Helper()
	body := resultTakeoverRequest{ClientRequestID: uuidToString(dbid.NewV7()), ResultTaskID: uuidToString(f.result.ID), FromReturnID: uuidToString(f.address.ID), ExpectedCurrentReturnID: uuidToString(f.address.ID), Target: resultTakeoverTarget{Kind: kind}}
	if kind == delegation.KindExternalSession {
		body.Target.ExternalSessionID = selector
	} else {
		body.Target.SourceTaskID = selector
		source, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(selector))
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := testHandler.Queries.GetAgentRuntime(context.Background(), source.RuntimeID)
		if err != nil {
			t.Fatal(err)
		}
		scope, err := delegation.ManagedSourceTaskScope(context.Background(), testHandler.Queries, source, f.address.WorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		body.Target.SelectionVersion = delegation.TakeoverSelectionVersion(db.IssueDelegationReturn{WorkspaceID: f.address.WorkspaceID, DispatcherAgentID: source.AgentID, RuntimeID: source.RuntimeID, Provider: pgtype.Text{String: runtime.Provider, Valid: true}, SessionID: source.SessionID, WorkDir: source.WorkDir, ScopeTaskID: scope}, source)
	}
	return body
}

func callTakeover(t *testing.T, f takeoverFixture, body resultTakeoverRequest) *httptest.ResponseRecorder {
	t.Helper()
	return serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.TakeoverDelegationResult), "POST", "/api/issues/"+uuidToString(f.result.IssueID)+"/result-takeovers", body, map[string]string{"id": uuidToString(f.result.IssueID)}, nil)
}

func decodeTakeover(t *testing.T, w *httptest.ResponseRecorder, status int) ResultTakeoverResponse {
	t.Helper()
	if w.Code != status {
		t.Fatalf("takeover HTTP%d want%d: %s", w.Code, status, w.Body.String())
	}
	var out ResultTakeoverResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResultTakeoverExternalPreservesUnknownAndReusesTarget(t *testing.T) {
	f := externalTakeoverFixture(t)
	ctx := context.Background()
	before, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: f.result.ID, DelegationReturnID: f.address.ID})
	if err != nil {
		t.Fatal(err)
	}
	resultBefore, _ := testHandler.Queries.GetAgentTask(ctx, f.result.ID)
	b := takeoverBody(t, f, delegation.KindExternalSession, takeoverExternalSession(t, f, "B"))
	accepted := decodeTakeover(t, callTakeover(t, f, b), 201)
	if accepted.TargetReturnID == uuidToString(f.address.ID) {
		t.Fatal("takeover rewrote original address")
	}
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	replay := decodeTakeover(t, callTakeover(t, f, b), 200)
	if replay.ID != accepted.ID {
		t.Fatal("request replay created an action")
	}
	bAgain := b
	bAgain.ClientRequestID = uuidToString(dbid.NewV7())
	same := decodeTakeover(t, callTakeover(t, f, bAgain), 201)
	if same.TargetReturnID != accepted.TargetReturnID {
		t.Fatal("same physical target was cloned")
	}
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, f.result.ID); n != 2 {
		t.Fatalf("fresh request duplicated notification: %d", n)
	}
	c := takeoverBody(t, f, delegation.KindExternalSession, takeoverExternalSession(t, f, "C"))
	c.ExpectedCurrentReturnID = accepted.TargetReturnID
	next := decodeTakeover(t, callTakeover(t, f, c), 201)
	replay = decodeTakeover(t, callTakeover(t, f, b), 200)
	if replay.TargetReturnID != accepted.TargetReturnID || replay.CurrentReturnID != next.TargetReturnID {
		t.Fatal("replay confused stable action target with current routing")
	}
	changed := b
	changed.Target.ExternalSessionID = c.Target.ExternalSessionID
	if got := callTakeover(t, f, changed); got.Code != 409 {
		t.Fatalf("conflicting replay %d", got.Code)
	}
	bAgain.ClientRequestID = uuidToString(dbid.NewV7())
	bAgain.ExpectedCurrentReturnID = next.TargetReturnID
	back := decodeTakeover(t, callTakeover(t, f, bAgain), 201)
	if back.TargetReturnID != accepted.TargetReturnID {
		t.Fatal("historical address was cloned")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2`, f.result.ID, parseUUID(accepted.TargetReturnID)); n != 1 {
		t.Fatal("switching back resent an existing notification")
	}
	after, _ := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: f.result.ID, DelegationReturnID: f.address.ID})
	if after.NotificationDeliveryID != before.NotificationDeliveryID || after.TakeoverID.Valid {
		t.Fatal("original input relation changed")
	}
	var state, target string
	dbfx.QueryRow(t, `SELECT delivery_status,target_session_id::text FROM agent_mailbox_message WHERE id=$1`, before.NotificationDeliveryID).Scan(&state, &target)
	if state != "unknown" || target != uuidToString(f.address.ExternalSessionID) {
		t.Fatal("original unknown envelope changed")
	}
	resultAfter, _ := testHandler.Queries.GetAgentTask(ctx, f.result.ID)
	if string(resultBefore.Result) != string(resultAfter.Result) || countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, f.result.IssueID, f.result.AgentID) != 1 {
		t.Fatal("takeover reran executor or changed result")
	}
	carried, err := delegation.RunReturns(ctx, testHandler.Queries, f.result.ID)
	if err != nil || len(carried) != 1 || carried[0].ID != f.address.ID {
		t.Fatalf("takeover treated as carried input: %v %v", carried, err)
	}
}

func TestResultTakeoverManagedRequiresCurrentPinAndReusesProvenance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, address := resultFixture(t, "takeover-managed")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, address, testUserID, token}
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	b := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "new-session-B", "work_dir": "/work/new-B", "resource_control_required": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	body := takeoverBody(t, f, delegation.KindManagedTask, b)
	if got := callTakeover(t, f, body); got.Code != 409 {
		t.Fatalf("unconfirmed source admitted: %d %s", got.Code, got.Body.String())
	}
	pin := withURLParam(newOwnerDaemonRequest("POST", "/session", map[string]string{"session_id": "new-session-B", "work_dir": "/work/new-B", "generation": queryString(t, `SELECT session_pin_generation::text FROM agent_task_queue WHERE id=$1`, b)}), "taskId", b)
	testutil.Call(t, testHandler.PinTaskSession, pin).Want(204)
	accepted := decodeTakeover(t, callTakeover(t, f, body), 201)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	link, err := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: parseUUID(accepted.TargetReturnID)})
	if err != nil || !link.NotificationTaskID.Valid {
		t.Fatalf("managed obligation did not materialize: %+v %v", link, err)
	}
	handling, err := testHandler.Queries.GetAgentTask(ctx, link.NotificationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, handling)
	if err != nil || turn == nil || turn.SessionID != "new-session-B" || turn.SourceTaskID != b || turn.ResultTaskID != uuidToString(result.ID) {
		t.Fatalf("strict target mismatch %+v %v", turn, err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',terminal_reported_at=now() WHERE id=$1`, b)
	originalLink, _ := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: address.ID})
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',started_at=now(),failure_reason='delegation_result_input_unknown',terminal_reported_at=now() WHERE id=$1`, originalLink.NotificationTaskID)
	claimReq := withURLParam(newOwnerDaemonRequest("POST", "/claim", nil), "runtimeId", uuidToString(source.RuntimeID))
	claimReq.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	var claim struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, claimReq).Want(200).JSON(&claim)
	if claim.Task == nil || claim.Task.ID != uuidToString(handling.ID) || !claim.Task.RequireResume {
		t.Fatal("takeover did not claim its exact strict handling turn")
	}
	handling, _ = testHandler.Queries.GetAgentTask(ctx, handling.ID)
	testutil.Call(t, testHandler.StartTask, startClaimRequest(uuidToString(handling.ID), uuidToString(handling.RuntimeID), handling.DispatchedAt.Time)).Want(200)
	testutil.Call(t, testHandler.CompleteTask, withURLParam(newOwnerDaemonRequest("POST", "/complete", map[string]string{"output": "handled saved result", "session_id": "new-session-B", "work_dir": "/work/new-B"}), "taskId", uuidToString(handling.ID))).Want(200)
	replay := decodeTakeover(t, callTakeover(t, f, body), 200)
	if replay.ID != accepted.ID {
		t.Fatal("ended B prevented exact replay")
	}
	fresh := body
	fresh.ClientRequestID = uuidToString(dbid.NewV7())
	fresh.ExpectedCurrentReturnID = accepted.TargetReturnID
	if got := callTakeover(t, f, fresh); got.Code != 409 {
		t.Fatalf("ended source authorized new action: %d", got.Code)
	}
	b2 := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "new-session-B", "work_dir": "/work/new-B", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	fresh.Target.SourceTaskID = b2
	fresh.Target.SelectionVersion = takeoverBody(t, f, delegation.KindManagedTask, b2).Target.SelectionVersion
	reused := decodeTakeover(t, callTakeover(t, f, fresh), 201)
	if reused.TargetReturnID != accepted.TargetReturnID {
		t.Fatal("new source turn cloned same physical address")
	}
	snapshot, _ := testHandler.Queries.GetDelegationReturnByID(ctx, parseUUID(reused.TargetReturnID))
	if uuidToString(snapshot.SourceTaskID) != b {
		t.Fatal("immutable provenance changed")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2`, result.ID, snapshot.ID); n != 1 {
		t.Fatal("same tuple received twice")
	}
	changed := body
	changed.Target.SourceTaskID = b2
	if got := callTakeover(t, f, changed); got.Code != 409 {
		t.Fatal("selector change accepted as same request")
	}
	beforeActions := countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id=$1`, b)
	fresh.ClientRequestID = uuidToString(dbid.NewV7())
	if got := callTakeover(t, f, fresh); got.Code != 409 {
		t.Fatalf("missing historical provenance was guessed or cloned: %d %s", got.Code, got.Body.String())
	}
	if countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID) != beforeActions {
		t.Fatal("invalid historical proof left an action")
	}
	decodeTakeover(t, callTakeover(t, f, body), 200)
}

func queryString(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var value string
	dbfx.QueryRow(t, sql, args...).Scan(&value)
	return value
}

func TestResultTakeoverTransactionAndCompetingTargets(t *testing.T) {
	f := externalTakeoverFixture(t)
	b := takeoverBody(t, f, delegation.KindExternalSession, takeoverExternalSession(t, f, "atomic-B"))
	fn := "takeover_fail_" + strings.ReplaceAll(uuidToString(f.result.ID), "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN IF NEW.task_id='%s' AND NEW.takeover_id IS NOT NULL THEN RAISE EXCEPTION 'takeover rollback probe'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql; CREATE TRIGGER %s BEFORE INSERT ON agent_task_delegation_return FOR EACH ROW EXECUTE FUNCTION %s();`, fn, uuidToString(f.result.ID), fn, fn))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)
	if got := callTakeover(t, f, b); got.Code != 500 {
		t.Fatalf("probe did not fail: %d %s", got.Code, got.Body.String())
	}
	if countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, f.result.ID) != 0 || countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, f.result.IssueID) != 1 {
		t.Fatal("failed action left partial state")
	}
	current := queryString(t, `SELECT session_id::text FROM external_agent_session_binding WHERE issue_id=$1 AND revoked_at IS NULL`, f.result.IssueID)
	if current != uuidToString(f.address.ExternalSessionID) {
		t.Fatal("failed action changed future route")
	}
	drop()
	c := takeoverBody(t, f, delegation.KindExternalSession, takeoverExternalSession(t, f, "atomic-C"))
	var wg sync.WaitGroup
	outputs := make(chan *httptest.ResponseRecorder, 2)
	for _, body := range []resultTakeoverRequest{b, c} {
		wg.Add(1)
		go func() { defer wg.Done(); outputs <- callTakeover(t, f, body) }()
	}
	wg.Wait()
	close(outputs)
	wins, conflicts := 0, 0
	for out := range outputs {
		switch out.Code {
		case 201:
			wins++
		case 409:
			conflicts++
		default:
			t.Fatalf("concurrent HTTP%d %s", out.Code, out.Body.String())
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", wins, conflicts)
	}
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := testHandler.TaskService.ReconcileDelegationResults(context.Background(), 100)
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
	if countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_delivery_id IS NOT NULL`, f.result.ID) != 2 {
		t.Fatal("dual consumers duplicated or lost takeover")
	}
}

func TestResultTakeoverDoesNotChangeInheritedExecutionResponsibility(t *testing.T) {
	f := externalTakeoverFixture(t)
	ctx := context.Background()
	other, session, _ := externalAgentFixture(t, "second carried dispatcher", "takeover-host")
	inherited, err := delegation.Ensure(ctx, testHandler.Queries, testWorkspaceID, uuidToString(f.result.IssueID), delegation.Address{DispatcherAgentID: other, Kind: delegation.KindExternalSession, ExternalSessionID: session}, testUserID)
	if err != nil {
		t.Fatal(err)
	}
	if err := delegation.LinkRun(ctx, testHandler.Queries, f.result.ID, delegation.ReturnIntent{ID: inherited.ID}); err != nil {
		t.Fatal(err)
	}
	bSession := takeoverExternalSession(t, f, "rerun-B")
	action := decodeTakeover(t, callTakeover(t, f, takeoverBody(t, f, delegation.KindExternalSession, bSession)), 201)
	bReturn, err := testHandler.Queries.GetDelegationReturnByID(ctx, parseUUID(action.TargetReturnID))
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/issues/" + uuidToString(f.result.IssueID) + "/rerun"
	params := map[string]string{"id": uuidToString(f.result.IssueID)}
	body := map[string]string{"task_id": uuidToString(f.result.ID)}
	human := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.RerunIssue), "POST", path, body, params, nil)
	if human.Code != 202 {
		t.Fatalf("human rerun %d %s", human.Code, human.Body.String())
	}
	var run AgentTaskResponse
	if err := json.Unmarshal(human.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	requireTaskReturns(t, run.ID, f.address, inherited)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',max_attempts=3,started_at=now() WHERE id=$1`, run.ID)
	if _, err := testHandler.TaskService.FailTask(service.WithTerminalReport(ctx), parseUUID(run.ID), "network", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatal(err)
	}
	retry, err := testHandler.Queries.GetAutomaticRetryChild(ctx, parseUUID(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	requireTaskReturns(t, uuidToString(retry.ID), f.address, inherited)
	// Exercise both durable propagation operators with a parent that has an
	// actual post-result notification. Neither may copy it as an input.
	if _, err := testHandler.Queries.CopyRetryDelegationReturns(ctx, db.CopyRetryDelegationReturnsParams{ParentTaskID: f.result.ID, ChildTaskID: retry.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.Queries.PropagateRetryDelegationReturns(ctx, 100); err != nil {
		t.Fatal(err)
	}
	requireTaskReturns(t, uuidToString(retry.ID), f.address, inherited)
	bToken := externalSessionTokenFixture(t, uuidToString(f.address.DispatcherAgentID), bSession, f.owner, time.Now().Add(time.Hour))
	explicit := serveAuthenticated(t, bToken, http.HandlerFunc(testHandler.RerunIssue), "POST", path, body, params, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, bSession) })
	if explicit.Code != 202 {
		t.Fatalf("explicit B rerun %d %s", explicit.Code, explicit.Body.String())
	}
	if err := json.Unmarshal(explicit.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	requireTaskReturns(t, run.ID, bReturn, inherited)
	requireTaskReturns(t, uuidToString(f.result.ID), f.address, inherited)
	link, _ := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: f.result.ID, DelegationReturnID: bReturn.ID})
	if uuidToString(link.TakeoverID) != action.ID {
		t.Fatal("new input rewrote original takeover provenance")
	}
	// A late accepted input keeps the original address throughout automatic
	// retries even though this actor now uses B for new explicit dispatches.
	if err := delegation.LinkRun(ctx, testHandler.Queries, parseUUID(queryString(t, `SELECT retry_of_task_id::text FROM agent_task_queue WHERE id=$1`, retry.ID)), delegation.ReturnIntent{ID: f.address.ID}); err != nil {
		t.Fatal(err)
	}
	requireTaskReturns(t, uuidToString(retry.ID), f.address, inherited)
}

func TestResultTakeoverAuthorizationAndTargetScope(t *testing.T) {
	f := externalTakeoverFixture(t)
	b := takeoverBody(t, f, delegation.KindExternalSession, takeoverExternalSession(t, f, "permission-B"))
	stranger := dbfx.User(t, "takeover stranger", fmt.Sprintf("takeover-%d@example.com", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, stranger, "member")
	denied := withURLParam(newRequestAs(stranger, "POST", "/takeover", b), "id", uuidToString(f.result.IssueID))
	testutil.Call(t, testHandler.TakeoverDelegationResult, denied).Want(403)
	machine := externalTokenFixture(t, uuidToString(f.address.DispatcherAgentID), f.owner, time.Now().Add(time.Hour))
	forged := f
	forged.token = machine
	if got := callTakeover(t, forged, b); got.Code != 403 {
		t.Fatalf("agent borrowed human takeover rights: %d", got.Code)
	}
	_, foreign, _ := externalAgentFixture(t, "foreign target", "takeover-host")
	b.Target.ExternalSessionID = foreign
	if got := callTakeover(t, f, b); got.Code != 409 {
		t.Fatalf("foreign target %d %s", got.Code, got.Body.String())
	}
	if countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, f.result.ID) != 0 {
		t.Fatal("denial left action")
	}
	source, result, address := resultFixture(t, "takeover-private-chat")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	managed := takeoverFixture{result, address, testUserID, token}
	chat := dbfx.ChatSession(t, uuidToString(source.AgentID), testutil.Cols{"creator_id": stranger})
	target := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"chat_session_id": chat, "runtime_id": source.RuntimeID, "status": "running", "session_id": "private-chat-session", "work_dir": "/work/private-chat", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	if got := callTakeover(t, managed, takeoverBody(t, managed, delegation.KindManagedTask, target)); got.Code != 403 {
		t.Fatalf("canManageAgent bypassed private chat: %d %s", got.Code, got.Body.String())
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id=NULL,issue_id=$2,resource_control_required=false WHERE id=$1`, target, source.IssueID)
	if got := callTakeover(t, managed, takeoverBody(t, managed, delegation.KindManagedTask, target)); got.Code != 409 {
		t.Fatalf("legacy execution admitted: %d", got.Code)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET resource_control_required=true WHERE id=$1`, target)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider='qoder_cloud' WHERE id=$1`, source.RuntimeID)
	if got := callTakeover(t, managed, takeoverBody(t, managed, delegation.KindManagedTask, target)); got.Code != 409 {
		t.Fatalf("unsupported executor admitted: %d", got.Code)
	}
	if countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, result.IssueID) != 1 {
		t.Fatal("target denial left address version")
	}
}

func TestResultTakeoverPreservesOldUnknownResourceDependency(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, address := resultFixture(t, "takeover-retention")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, address, testUserID, token}
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	b := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "retained-B", "work_dir": "/work/retained-B", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	action := decodeTakeover(t, callTakeover(t, f, takeoverBody(t, f, delegation.KindManagedTask, b)), 201)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	old, _ := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: address.ID})
	newer, _ := testHandler.Queries.GetResultReturnObligation(ctx, db.GetResultReturnObligationParams{TaskID: result.ID, DelegationReturnID: parseUUID(action.TargetReturnID)})
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',started_at=now(),completed_at=now(),terminal_reported_at=now(),failure_reason='delegation_result_input_unknown' WHERE id=$1`, old.NotificationTaskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1 OR id=$2`, b, newer.NotificationTaskID)
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest("PATCH", "/issue", map[string]string{"status": "done"}), "id", uuidToString(result.IssueID))).Want(200)
	if err := service.RequireRuntimeDelegationResourcesReleased(ctx, testHandler.Queries, source.RuntimeID); !errors.Is(err, service.ErrDelegationResourcesRetained) {
		t.Fatalf("takeover discharged old unknown dependency: %v", err)
	}
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.ListTasksByIssue), "GET", "/api/issues/"+uuidToString(result.IssueID)+"/task-runs", nil, map[string]string{"id": uuidToString(result.IssueID)}, nil)
	if w.Code != 200 {
		t.Fatalf("task-runs %d %s", w.Code, w.Body.String())
	}
	var tasks []AgentTaskResponse
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	found := map[string]ResultNotificationResponse{}
	for _, task := range tasks {
		if task.ID == uuidToString(result.ID) && task.ResultNotifications != nil {
			for _, note := range *task.ResultNotifications {
				found[note.ReturnID] = note
			}
		}
	}
	if found[uuidToString(address.ID)].Status != "unknown" || found[action.TargetReturnID].Status != "received" || found[action.TargetReturnID].TakeoverID != action.ID || found[uuidToString(address.ID)].CurrentReturnID != action.TargetReturnID {
		t.Fatalf("projection lost original/new distinction: %+v", found)
	}
}

func TestResultTakeoverExternalUsesExistingDeliveryWorker(t *testing.T) {
	f := externalTakeoverFixture(t)
	ctx := context.Background()
	b := takeoverExternalSession(t, f, "worker-B")
	action := decodeTakeover(t, callTakeover(t, f, takeoverBody(t, f, delegation.KindExternalSession, b)), 201)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET next_attempt_at=now()+interval '1 hour' WHERE id=(SELECT notification_delivery_id FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2)`, f.result.ID, f.address.ID)
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	host := &resultWorkerHost{received: make(chan delivery.Delivery, 2), session: "session-worker-B"}
	client := &resultWorkerClient{queue: delivery.NewQueue(testHandler.Queries, testPool), daemon: "takeover-host", owner: f.owner, done: cancel}
	_ = delivery.NewWorker(client, host, func() []string { return []string{testWorkspaceID} }, client.daemon, nil).Run(runCtx)
	select {
	case delivered := <-host.received:
		var payload map[string]string
		if err := json.Unmarshal(delivered.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if delivered.Kind != "delegation_result" || delivered.Refs.IssueID != uuidToString(f.result.IssueID) || payload["result_task_id"] != uuidToString(f.result.ID) || payload["delegation_return_id"] != action.TargetReturnID || payload["outcome"] != "completed" {
			t.Fatalf("host got incorrect saved-result references: %+v", payload)
		}
		t.Logf("fake host received saved result=%s target return=%s outcome=%s", payload["result_task_id"], payload["delegation_return_id"], payload["outcome"])
	default:
		t.Fatal("takeover did not reach real delivery worker/fake host")
	}
	if state := queryString(t, `SELECT m.delivery_status FROM agent_mailbox_message m JOIN agent_task_delegation_return l ON l.notification_delivery_id=m.id WHERE l.task_id=$1 AND l.delegation_return_id=$2`, f.result.ID, f.address.ID); state != "unknown" {
		t.Fatal("new notification rewrote old host uncertainty")
	}
}
