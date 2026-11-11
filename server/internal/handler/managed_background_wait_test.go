package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type managedWaitFixture struct {
	agent, runtime, task, token, generation, id string
	address                                     protocol.WaitAddress
	request                                     protocol.WaitAuthorization
}

func newManagedWaitFixture(t *testing.T) managedWaitFixture {
	t.Helper()
	f := managedWaitFixture{generation: uuid.NewString(), id: uuid.NewString()}
	f.runtime = dbfx.Runtime(t, "Managed wait runtime", testutil.Cols{"provider": "claude", "runtime_mode": "local", "daemon_id": "managed-wait-daemon"})
	f.agent = dbfx.Agent(t, "Managed wait actor", f.runtime)
	f.task = dbfx.Task(t, f.agent, testutil.Cols{"runtime_id": f.runtime, "issue_id": dbfx.Issue(t, "Managed wait scope"), "status": "running", "session_id": "frozen-provider-session", "work_dir": "/owned/task/workdir", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": f.generation})
	f.token = taskTokenFixture(t, f.agent, f.task)
	f.request = protocol.WaitAuthorization{ID: f.id, DaemonID: "managed-wait-daemon", RequestHash: strings.Repeat("a", 64), Deadline: time.Now().UTC().Add(time.Hour)}
	w := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.AuthorizeBackgroundWait), "POST", "/api/background-waits/authorize", f.request, nil, nil)
	if w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &f.address); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE recipient_agent_id=$1", f.agent)
	return f
}

func (f managedWaitFixture) suspend(t *testing.T) {
	t.Helper()
	req := TaskCompleteRequest{TaskTurn: protocol.TaskTurn{Generation: f.generation, KnownWaitIDs: []string{f.id}}, Output: "waiting", SessionID: f.address.SessionID, WorkDir: f.address.WorkDir}
	r := withURLParam(newDaemonTokenRequest("POST", "/complete", req, testWorkspaceID, "managed-wait-daemon"), "taskId", f.task)
	testutil.Call(t, testHandler.CompleteTask, r).Want(200)
}

func (f managedWaitFixture) complete(t *testing.T) string {
	t.Helper()
	code := 7
	result := protocol.WaitResult{ID: f.id, Address: f.address, State: "failed", ExitCode: &code, FinishedAt: time.Unix(1700000000, 0).UTC(), LogPath: "/private/wait.log"}
	var out struct {
		ID string `json:"delivery_id"`
	}
	testutil.Call(t, testHandler.CompleteBackgroundWait, newDaemonTokenRequest("POST", "/complete", result, testWorkspaceID, "managed-wait-daemon")).Want(200).JSON(&out)
	return out.ID
}

func TestManagedBackgroundWaitSuspendsWithoutCompleting(t *testing.T) {
	f := newManagedWaitFixture(t)
	if f.address.TaskID != f.task || f.address.AgentID != f.agent {
		t.Fatalf("wrong frozen address: %+v", f.address)
	}
	f.suspend(t)
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "deferred" || task.FireAt.Valid || task.CompletedAt.Valid || task.TerminalReportedAt.Valid {
		t.Fatalf("suspension became completion: %+v", task)
	}
	decision, err := testHandler.TaskService.ResourceRelease(t.Context(), parseUUID(testWorkspaceID), "managed-wait-daemon", protocol.ResourceReleaseRequest{Kind: "task", SourceTaskID: f.task})
	if err != nil || decision.ReleaseAllowed {
		t.Fatalf("suspended resources released: %+v %v", decision, err)
	}
	first := f.complete(t)
	second := f.complete(t)
	if first == "" || first != second {
		t.Fatal("completion replay changed mailbox id")
	}
	task, err = testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "deferred" || !task.FireAt.Valid {
		t.Fatalf("ready wait did not use deferred promoter: %+v", task)
	}
	wait, err := service.VerifiedBackgroundWait(t.Context(), testHandler.Queries, task, mustWaitRuntime(t, f.runtime))
	if err != nil || wait == nil || wait.Address.SessionID != "frozen-provider-session" {
		t.Fatalf("resume proof: %+v %v", wait, err)
	}
}

func mustWaitRuntime(t *testing.T, id string) db.AgentRuntime {
	t.Helper()
	runtime, err := testHandler.Queries.GetAgentRuntime(t.Context(), parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestManagedBackgroundWaitEarlyReadSuppressesResume(t *testing.T) {
	f := newManagedWaitFixture(t)
	f.complete(t)
	w := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.ReadBackgroundWait), "POST", "/read", nil, map[string]string{"waitId": f.id}, nil)
	if w.Code != 200 {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	var status protocol.WaitStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.State != "failed" || status.LogPath != "/private/wait.log" {
		t.Fatalf("result: %+v %v", status, err)
	}
	f.suspend(t)
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" {
		t.Fatalf("already read result created another execution: %s", task.Status)
	}
}

func TestManagedBackgroundWaitRestartMissingJournalReportsLost(t *testing.T) {
	f := newManagedWaitFixture(t)
	if err := testHandler.TaskService.RecoverBackgroundWaits(t.Context(), parseUUID(f.runtime), nil); err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil {
		t.Fatal(err)
	}
	wait, err := service.TaskBackgroundWait(task)
	if err != nil || wait == nil || wait.Result == nil || wait.Result.State != "lost" || task.Status != "deferred" || !task.FireAt.Valid {
		t.Fatalf("recovery: %+v %+v %v", task, wait, err)
	}
}

func TestManagedBackgroundWaitMissingAdmissionBecomesLost(t *testing.T) {
	f := newManagedWaitFixture(t)
	req := TaskCompleteRequest{TaskTurn: protocol.TaskTurn{Generation: f.generation, KnownWaitIDs: []string{}}, Output: "registration response was lost"}
	r := withURLParam(newDaemonTokenRequest("POST", "/complete", req, testWorkspaceID, "managed-wait-daemon"), "taskId", f.task)
	testutil.Call(t, testHandler.CompleteTask, r).Want(200)
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	wait, waitErr := service.TaskBackgroundWait(task)
	if err != nil || waitErr != nil || wait == nil || wait.Result == nil || wait.Result.State != "lost" || task.Status != "deferred" || !task.FireAt.Valid || task.TerminalReportedAt.Valid {
		t.Fatalf("orphaned registration: task=%+v wait=%+v errors=%v,%v", task, wait, err, waitErr)
	}
}

func TestManagedBackgroundWaitStaleTurnCannotFinishResume(t *testing.T) {
	f := newManagedWaitFixture(t)
	f.complete(t)
	f.suspend(t)
	next := uuid.NewString()
	dbfx.Exec(t, "UPDATE agent_task_queue SET status='running',session_pin_generation=$1 WHERE id=$2", next, f.task)
	w := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.ReadBackgroundWait), "POST", "/read", nil, map[string]string{"waitId": f.id}, nil)
	if w.Code != 200 {
		t.Fatalf("consume: %d %s", w.Code, w.Body.String())
	}
	for _, fail := range []bool{false, true} {
		var fn http.HandlerFunc = testHandler.CompleteTask
		var body any = TaskCompleteRequest{TaskTurn: protocol.TaskTurn{Generation: f.generation}, Output: "stale completion"}
		if fail {
			fn = testHandler.FailTask
			body = TaskFailRequest{TaskTurn: protocol.TaskTurn{Generation: f.generation}, Error: "stale failure"}
		}
		r := withURLParam(newDaemonTokenRequest("POST", "/terminal", body, testWorkspaceID, "managed-wait-daemon"), "taskId", f.task)
		testutil.Call(t, fn, r).Want(200)
	}
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil || task.Status != "running" || task.TerminalReportedAt.Valid {
		t.Fatalf("stale turn ended resumed execution: %+v %v", task, err)
	}
	req := TaskCompleteRequest{TaskTurn: protocol.TaskTurn{Generation: next}, Output: "resumed delivery", SessionID: "must-not-retarget", WorkDir: "/wrong", RetiredSessionID: f.address.SessionID, SessionRolloutMissing: true}
	r := withURLParam(newDaemonTokenRequest("POST", "/complete", req, testWorkspaceID, "managed-wait-daemon"), "taskId", f.task)
	testutil.Call(t, testHandler.CompleteTask, r).Want(200)
	task, err = testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil || task.Status != "completed" || task.SessionID.String != f.address.SessionID || task.WorkDir.String != f.address.WorkDir {
		t.Fatalf("final turn lost frozen address: %+v %v", task, err)
	}
}

func TestManagedBackgroundWaitScopeAndExpiredCredential(t *testing.T) {
	f := newManagedWaitFixture(t)
	otherTask := dbfx.Task(t, f.agent, testutil.Cols{"runtime_id": f.runtime, "issue_id": dbfx.Issue(t, "Other task"), "status": "running"})
	otherToken := taskTokenFixture(t, f.agent, otherTask)
	w := serveAuthenticated(t, otherToken, http.HandlerFunc(testHandler.ReadBackgroundWait), "POST", "/read", nil, map[string]string{"waitId": f.id}, nil)
	if w.Code != 404 {
		t.Fatalf("cross task read: %d %s", w.Code, w.Body.String())
	}
	changed := f.request
	changed.RequestHash = strings.Repeat("b", 64)
	w = serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.AuthorizeBackgroundWait), "POST", "/authorize", changed, nil, nil)
	if w.Code != 409 {
		t.Fatalf("changed registration: %d %s", w.Code, w.Body.String())
	}
	dbfx.Exec(t, "UPDATE task_token SET expires_at=now()-interval '1 second' WHERE task_id=$1", f.task)
	w = serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.ReadBackgroundWait), "POST", "/access", nil, map[string]string{"waitId": f.id}, nil)
	if w.Code != 401 {
		t.Fatalf("expired credential: %d %s", w.Code, w.Body.String())
	}
	f.suspend(t)
	f.complete(t)
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil || task.Status != "deferred" || !task.FireAt.Valid {
		t.Fatalf("token expiry stopped registered wait: %+v %v", task, err)
	}
}

func TestManagedBackgroundWaitSurvivesOfflineSweepers(t *testing.T) {
	f := newManagedWaitFixture(t)
	ordinary := dbfx.Task(t, f.agent, testutil.Cols{"runtime_id": f.runtime, "issue_id": dbfx.Issue(t, "Ordinary stale task"), "status": "running", "started_at": time.Now().Add(-time.Hour)})
	dbfx.Exec(t, "UPDATE agent_runtime SET status='offline',last_seen_at=now()-interval '3 hours' WHERE id=$1", f.runtime)
	dbfx.Exec(t, "UPDATE agent_task_queue SET started_at=now()-interval '3 hours' WHERE id=$1", f.task)
	if _, err := testHandler.Queries.FailStaleTasks(t.Context(), db.FailStaleTasksParams{DispatchTimeoutSecs: 1, RunningTimeoutSecs: 1, RuntimeStaleSecs: 1, RuntimeReconnectGraceSecs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.Queries.FailTasksForOfflineRuntimes(t.Context(), db.FailTasksForOfflineRuntimesParams{ReconnectGraceSecs: 1, MaxPerTick: 100}); err != nil {
		t.Fatal(err)
	}
	waitTask, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil || waitTask.Status != "running" {
		t.Fatalf("sweeper lost continuation: %s %v", waitTask.Status, err)
	}
	other, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(ordinary))
	if err != nil || other.Status != "failed" {
		t.Fatalf("ordinary stale task changed policy: %s %v", other.Status, err)
	}
	if err := testHandler.TaskService.RecoverBackgroundWaits(t.Context(), parseUUID(f.runtime), []string{f.task + ":" + f.id}); err != nil {
		t.Fatal(err)
	}
	waitTask, err = testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	wait, waitErr := service.TaskBackgroundWait(waitTask)
	if err != nil || waitErr != nil || waitTask.Status != "deferred" || wait.Result != nil || waitTask.FireAt.Valid {
		t.Fatalf("known journal was lost: %s %+v %v %v", waitTask.Status, wait, err, waitErr)
	}
	f.complete(t)
}

func TestManagedBackgroundWaitOldReadDoesNotConsumeNewRegistration(t *testing.T) {
	f := newManagedWaitFixture(t)
	f.complete(t)
	w := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.ReadBackgroundWait), "POST", "/read", nil, map[string]string{"waitId": f.id}, nil)
	if w.Code != 200 {
		t.Fatalf("consume original: %d %s", w.Code, w.Body.String())
	}
	next := f.request
	next.ID = uuid.NewString()
	w = serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.AuthorizeBackgroundWait), "POST", "/authorize", next, nil, nil)
	if w.Code != 200 {
		t.Fatalf("next registration: %d %s", w.Code, w.Body.String())
	}
	f.complete(t)
	w = serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.ReadBackgroundWait), "POST", "/read", nil, map[string]string{"waitId": f.id}, nil)
	if w.Code != 200 {
		t.Fatalf("old result replay: %d %s", w.Code, w.Body.String())
	}
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	wait, waitErr := service.TaskBackgroundWait(task)
	if err != nil || waitErr != nil || wait.ID != next.ID || wait.Consumed || wait.Result != nil {
		t.Fatalf("old result mutated new wait: %+v %v %v", wait, err, waitErr)
	}
}
