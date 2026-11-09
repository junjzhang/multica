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
	r := withURLParam(newDaemonTokenRequest("POST", "/suspend", protocol.WaitSuspend{Generation: f.generation, SessionID: f.address.SessionID, WorkDir: f.address.WorkDir}, testWorkspaceID, "managed-wait-daemon"), "taskId", f.task)
	testutil.Call(t, testHandler.SuspendBackgroundWait, r).Want(200)
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
	r := withURLParam(newDaemonTokenRequest("POST", "/complete", TaskCompleteRequest{Output: "waiting", SessionID: f.address.SessionID, WorkDir: f.address.WorkDir}, testWorkspaceID, "managed-wait-daemon"), "taskId", f.task)
	testutil.Call(t, testHandler.CompleteTask, r).Want(409)
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
	if task.Status != "running" {
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
