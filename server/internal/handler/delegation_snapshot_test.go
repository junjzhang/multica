package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func managedSnapshotPinBody(t *testing.T, source, session, dir string) map[string]any {
	t.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(source))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"session_id": session, "work_dir": dir, "generation": uuidToString(task.SessionPinGeneration)}
}

func pinManagedSnapshotSource(t *testing.T, source, session, dir string) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.PinTaskSession(w, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/pin-session",
		managedSnapshotPinBody(t, source, session, dir)), "taskId", source))
	if w.Code != http.StatusNoContent {
		t.Fatalf("pin = %d: %s", w.Code, w.Body.String())
	}
}

func createSnapshotIssue(t *testing.T, token, target, title string) string {
	t.Helper()
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost,
		"/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("created issue missing")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	return issueID
}

func requireFrozenManagedSnapshot(t *testing.T, got, want db.IssueDelegationReturn) {
	t.Helper()
	if got.ID != want.ID || got.SourceTaskID != want.SourceTaskID || got.RuntimeID != want.RuntimeID ||
		got.Provider != want.Provider || got.SessionID != want.SessionID || got.WorkDir != want.WorkDir {
		t.Fatalf("accepted address changed: got=%+v want=%+v", got, want)
	}
}

func TestManagedReturnSnapshotSurvivesRepinAndCompletion(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, source, token := managedDispatcherFixture(t, "snapshot-repin")
	x, _ := executableTarget(t, "snapshot-repin-x")
	y, _ := executableTarget(t, "snapshot-repin-y")
	pinManagedSnapshotSource(t, source, "snapshot-session-a", "/work/snapshot-a")
	issueID := createSnapshotIssue(t, token, x, fmt.Sprintf("Snapshot repin %d", time.Now().UnixNano()))
	accepted, ok := acceptedReturn(t, issueID)
	if !ok {
		t.Fatal("accepted return missing")
	}
	sourceTask, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(source))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := testHandler.Queries.GetAgentRuntime(context.Background(), sourceTask.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.RuntimeID != sourceTask.RuntimeID || accepted.Provider.String != runtime.Provider ||
		accepted.SessionID.String != "snapshot-session-a" || accepted.WorkDir.String != "/work/snapshot-a" {
		t.Fatalf("snapshot did not capture source runtime and pin: %+v", accepted)
	}

	pinManagedSnapshotSource(t, source, "snapshot-session-b", "/work/snapshot-b")
	current, _ := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(source))
	if current.SessionID.String != "snapshot-session-b" || current.WorkDir.String != "/work/snapshot-b" {
		t.Fatal("ordinary task repin was incorrectly frozen")
	}
	rows := taskReturnIDs(t, latestTaskIDFor(t, issueID, x))
	if len(rows) != 1 {
		t.Fatalf("child returns = %d", len(rows))
	}
	requireFrozenManagedSnapshot(t, rows[0], accepted)
	content := fmt.Sprintf("[@Y](mention://agent/%s) after source repin", y)
	code, _, body := intentComment(t, token, issueID, content, "")
	if code != http.StatusConflict || !strings.Contains(body, "delegation_return_conflict") {
		t.Fatalf("changed address accepted: %d %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND content=$2`, issueID, content); n != 0 {
		t.Fatalf("conflict saved %d comments", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, y); n != 0 {
		t.Fatalf("conflict created %d runs", n)
	}

	w := httptest.NewRecorder()
	testHandler.CompleteTask(w, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/complete",
		map[string]any{"output": "ordinary fallback completed", "session_id": "snapshot-session-b", "work_dir": "/work/snapshot-b"}), "taskId", source))
	if w.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", w.Code, w.Body.String())
	}
	current, _ = testHandler.Queries.GetAgentTask(context.Background(), parseUUID(source))
	if current.Status != "completed" || current.SessionID.String != "snapshot-session-b" {
		t.Fatalf("ordinary completion changed: %+v", current)
	}
	after, _ := acceptedReturn(t, issueID)
	requireFrozenManagedSnapshot(t, after, accepted)
	code, _, body = intentComment(t, token, issueID, content+" after completion", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("completed source token remained usable: %d %s", code, body)
	}
}

func TestManagedReturnSnapshotRejectsEveryAddressChange(t *testing.T) {
	for _, field := range []string{"session", "work_dir", "runtime", "provider"} {
		t.Run(field, func(t *testing.T) {
			if testHandler == nil || testPool == nil {
				t.Skip("database not available")
			}
			dispatcher, source, token := managedDispatcherFixture(t, "snapshot-change-"+field)
			x, _ := executableTarget(t, "snapshot-change-x-"+field)
			y, _ := executableTarget(t, "snapshot-change-y-"+field)
			issueID := createSnapshotIssue(t, token, x, fmt.Sprintf("Snapshot %s %d", field, time.Now().UnixNano()))
			accepted, _ := acceptedReturn(t, issueID)
			switch field {
			case "session":
				pinManagedSnapshotSource(t, source, "different-session", accepted.WorkDir.String)
			case "work_dir":
				pinManagedSnapshotSource(t, source, accepted.SessionID.String, "/different/work-dir")
			case "runtime":
				other := dbfx.Runtime(t, "Snapshot replacement runtime")
				dbfx.Exec(t, `UPDATE agent SET runtime_id=$2 WHERE id=$1`, dispatcher, other)
				dbfx.Exec(t, `UPDATE agent_task_queue SET runtime_id=$2 WHERE id=$1`, source, other)
			case "provider":
				dbfx.Exec(t, `UPDATE agent_runtime SET provider='different-provider' WHERE id=$1`, accepted.RuntimeID)
			}
			code, _, body := intentComment(t, token, issueID, fmt.Sprintf("[@Y](mention://agent/%s) changed %s", y, field), "")
			if code != http.StatusConflict || !strings.Contains(body, "delegation_return_conflict") {
				t.Fatalf("changed %s accepted: %d %s", field, code, body)
			}
			after, _ := acceptedReturn(t, issueID)
			requireFrozenManagedSnapshot(t, after, accepted)
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, y); n != 0 {
				t.Fatalf("changed %s created %d runs", field, n)
			}
		})
	}
}

func TestManagedReturnSnapshotReusesSameAddressAfterSourceChanges(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, source, token := managedDispatcherFixture(t, "snapshot-continuation")
	x, _ := executableTarget(t, "snapshot-continuation-x")
	y, _ := executableTarget(t, "snapshot-continuation-y")
	issueID := createSnapshotIssue(t, token, x, fmt.Sprintf("Snapshot continuation %d", time.Now().UnixNano()))
	accepted, _ := acceptedReturn(t, issueID)
	pinManagedSnapshotSource(t, source, "ordinary-fallback-session", "/work/fallback")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, source)
	turn := dbfx.Task(t, dispatcher, testutil.Cols{"runtime_id": uuidToString(accepted.RuntimeID), "status": "running", "session_id": accepted.SessionID.String, "work_dir": accepted.WorkDir.String, "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	newToken := taskTokenFixture(t, dispatcher, turn)
	code, _, body := intentComment(t, newToken, issueID, fmt.Sprintf("[@Y](mention://agent/%s) original physical address continued", y), "")
	if code != http.StatusCreated {
		t.Fatalf("same-address new turn refused: %d %s", code, body)
	}
	after, _ := acceptedReturn(t, issueID)
	requireFrozenManagedSnapshot(t, after, accepted)
	requireTaskReturns(t, latestTaskIDFor(t, issueID, y), accepted)
}

func TestManagedReturnSnapshotSerializesFirstAcceptanceWithPin(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, source, token := managedDispatcherFixture(t, "snapshot-accept-lock")
	x, _ := executableTarget(t, "snapshot-accept-lock-x")
	issueID := dbfx.Issue(t, "Snapshot acceptance lock")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	key := int64(760000 + time.Now().UnixNano()%100000)
	fn := "snapshot_accept_gate_" + strings.ReplaceAll(issueID, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.issue_id='%s' THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON issue_delegation_return FOR EACH ROW EXECUTE FUNCTION %s();`, fn, issueID, key, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON issue_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	gate := holdAdvisoryLock(t, key)
	acceptedDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		acceptedDone <- serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
			"/api/issues/"+issueID+"/comments", map[string]any{"content": fmt.Sprintf("[@X](mention://agent/%s) accept before repin", x)}, map[string]string{"id": issueID}, nil)
	}()
	waitForBackendBlockedBy(t, gate.holderPID(t))
	var acceptingPID int32
	if err := testPool.QueryRow(context.Background(), `SELECT pid FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)) LIMIT 1`, gate.holderPID(t)).Scan(&acceptingPID); err != nil {
		t.Fatal(err)
	}
	pinDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.PinTaskSession(w, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/pin-session", managedSnapshotPinBody(t, source, "later-pin", "/work/later-pin")), "taskId", source))
		pinDone <- w
	}()
	waitForBackendBlockedBy(t, acceptingPID)
	gate.release()
	w := waitForResponse(t, acceptedDone)
	if w.Code != http.StatusCreated {
		t.Fatalf("accept = %d: %s", w.Code, w.Body.String())
	}
	w = waitForResponse(t, pinDone)
	if w.Code != http.StatusNoContent {
		t.Fatalf("repin = %d: %s", w.Code, w.Body.String())
	}
	accepted, _ := acceptedReturn(t, issueID)
	if accepted.SessionID.String != "dispatcher-session-snapshot-accept-lock" || accepted.WorkDir.String != "/work/dispatcher-snapshot-accept-lock" {
		t.Fatalf("acceptance took later pin: %+v", accepted)
	}
	current, _ := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(source))
	if current.SessionID.String != "later-pin" {
		t.Fatal("ordinary pin did not finish after acceptance")
	}
}

func TestManagedReturnSnapshotUsesLockedSourceRuntimeProvider(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, source, token := managedDispatcherFixture(t, "snapshot-provider-lock")
	x, _ := executableTarget(t, "snapshot-provider-lock-x")
	issueID := dbfx.Issue(t, "Snapshot provider lock")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	ctx := context.Background()
	sourceTask, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(source))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE agent_runtime SET provider='source-provider-after-update' WHERE id=$1`, sourceTask.RuntimeID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
			"/api/issues/"+issueID+"/comments", map[string]any{"content": fmt.Sprintf("[@X](mention://agent/%s) while source runtime changes", x)}, map[string]string{"id": issueID}, nil)
	}()
	waitForBackendBlockedBy(t, lockHolderPID(t, tx))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := waitForResponse(t, done)
	if w.Code != http.StatusCreated {
		t.Fatalf("accept = %d: %s", w.Code, w.Body.String())
	}
	accepted, _ := acceptedReturn(t, issueID)
	if accepted.RuntimeID != sourceTask.RuntimeID || accepted.Provider.String != "source-provider-after-update" {
		t.Fatalf("snapshot used an unlocked or different runtime: %+v", accepted)
	}
}
