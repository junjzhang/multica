package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestWaitReceiptBatchReadOnlyAndSessionIsolation(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Receipt read", "receipt-daemon")
	f := testutil.New(testPool, testWorkspaceID, owner)
	f.Runtime(t, "Receipt host", testutil.Cols{"daemon_id": "receipt-daemon"})
	token := externalSessionTokenFixture(t, agent, session, owner, time.Now().Add(time.Hour))
	a := protocol.WaitAddress{WorkspaceID: testWorkspaceID, AgentID: agent, SessionID: session, DaemonID: "receipt-daemon"}
	id := uuid.NewString()
	request := protocol.WaitDeliveryRequest{Waits: []protocol.WaitLookup{{ID: id, Address: a}}}
	read := func(req protocol.WaitDeliveryRequest, want int) protocol.WaitDeliveryResponse {
		t.Helper()
		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.BackgroundWaitStatuses), "POST", "/api/background-waits/status", req, nil, nil)
		if w.Code != want {
			t.Fatalf("receipt read=%d %s", w.Code, w.Body.String())
		}
		var out protocol.WaitDeliveryResponse
		if want == 200 && json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("invalid receipts")
		}
		return out
	}
	if status := read(request, 200).Statuses[id]; !status.Known || status.Enqueued {
		t.Fatalf("missing entry %+v", status)
	}
	result := protocol.WaitResult{ID: id, Address: a, State: "lost", FinishedAt: time.Now().UTC()}
	r := newDaemonTokenRequest("POST", "/complete", result, testWorkspaceID, "receipt-daemon")
	r = r.WithContext(middleware.WithDaemonTokenOwnerID(r.Context(), owner))
	var completed struct {
		ID string `json:"delivery_id"`
	}
	testutil.Call(t, testHandler.CompleteBackgroundWait, r).Want(200).JSON(&completed)
	dbfx.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE id=$1", completed.ID)
	for _, states := range []struct{ delivery, processing string }{{"pending", "unread"}, {"unknown", "unread"}, {"delivered", "read"}, {"delivered", "handled"}, {"failed", "unread"}} {
		dbfx.Exec(t, "UPDATE agent_mailbox_message SET delivery_status=$2,processing_status=$3 WHERE id=$1", completed.ID, states.delivery, states.processing)
		var before, after []byte
		if err := testPool.QueryRow(t.Context(), "SELECT row_to_json(m)::jsonb FROM agent_mailbox_message m WHERE id=$1", completed.ID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		status := read(request, 200).Statuses[id]
		if !status.Known || !status.Enqueued || status.DeliveryStatus != states.delivery || status.ProcessingStatus != states.processing {
			t.Fatalf("receipt %+v", status)
		}
		if err := testPool.QueryRow(t.Context(), "SELECT row_to_json(m)::jsonb FROM agent_mailbox_message m WHERE id=$1", completed.ID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("read mutated mailbox")
		}
	}
	for _, mutate := range []func(*protocol.WaitAddress){func(a *protocol.WaitAddress) { a.SessionID = uuid.NewString() }, func(a *protocol.WaitAddress) { a.WorkspaceID = uuid.NewString() }, func(a *protocol.WaitAddress) { a.AgentID = uuid.NewString() }, func(a *protocol.WaitAddress) { a.TaskID = uuid.NewString() }} {
		changed := request
		changed.Waits = append([]protocol.WaitLookup(nil), request.Waits...)
		mutate(&changed.Waits[0].Address)
		read(changed, 403)
	}
	// A different physical host cannot inspect a receipt by guessing its ID.
	wrongHost := request
	wrongHost.Waits = append([]protocol.WaitLookup(nil), request.Waits...)
	wrongHost.Waits[0].Address.DaemonID = "other-host"
	read(wrongHost, 404)
}

func TestManagedWaitReceiptDoesNotConsumeAndRejectsOtherTask(t *testing.T) {
	f := newManagedWaitFixture(t)
	f.complete(t)
	req := protocol.WaitDeliveryRequest{Waits: []protocol.WaitLookup{{ID: f.id, Address: f.address}}}
	w := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.BackgroundWaitStatuses), "POST", "/api/background-waits/status", req, nil, nil)
	if w.Code != 200 {
		t.Fatalf("managed receipt=%d %s", w.Code, w.Body.String())
	}
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.task))
	if err != nil {
		t.Fatal(err)
	}
	wait, err := service.TaskBackgroundWait(task)
	if err != nil || wait.Consumed {
		t.Fatal("receipt read consumed managed wait")
	}
	if f.address.IssueID != uuidToString(task.IssueID) {
		t.Fatal("managed issue metadata missing")
	}
	otherTask := dbfx.Task(t, f.agent, testutil.Cols{"runtime_id": f.runtime, "status": "running"})
	req.Waits[0].Address.TaskID = otherTask
	denied := serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.BackgroundWaitStatuses), "POST", "/api/background-waits/status", req, nil, nil)
	if denied.Code != 403 {
		t.Fatalf("cross task status=%d %s", denied.Code, denied.Body.String())
	}
}

func TestWaitDisplayIssueAuthorizationAndScope(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Display issue", "display-daemon")
	f := testutil.New(testPool, testWorkspaceID, owner)
	f.Runtime(t, "Display host", testutil.Cols{"daemon_id": "display-daemon"})
	issue := f.Issue(t, "Associated issue")
	token := externalSessionTokenFixture(t, agent, session, owner, time.Now().Add(time.Hour))
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.AuthorizeBackgroundWait), "POST", "/api/background-waits/authorize", protocol.WaitAuthorization{DaemonID: "display-daemon", IssueRef: issue}, nil, nil)
	if w.Code != 200 {
		t.Fatalf("issue auth=%d %s", w.Code, w.Body.String())
	}
	var a protocol.WaitAddress
	if json.Unmarshal(w.Body.Bytes(), &a) != nil || a.IssueID != issue {
		t.Fatal("issue not resolved")
	}
	missing := serveAuthenticated(t, token, http.HandlerFunc(testHandler.AuthorizeBackgroundWait), "POST", "/api/background-waits/authorize", protocol.WaitAuthorization{DaemonID: "display-daemon", IssueRef: uuid.NewString()}, nil, nil)
	if missing.Code != 404 {
		t.Fatalf("inaccessible issue=%d", missing.Code)
	}
	scope := serveAuthenticated(t, token, http.HandlerFunc(testHandler.GetBackgroundWaitScope), "GET", "/api/background-waits/scope?daemon_id=display-daemon", nil, nil, nil)
	if scope.Code != 200 {
		t.Fatalf("scope=%d %s", scope.Code, scope.Body.String())
	}
	var own protocol.WaitAddress
	if json.Unmarshal(scope.Body.Bytes(), &own) != nil || own.SessionID != session || own.IssueID != "" {
		t.Fatal("wrong read scope")
	}
	f.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "owner_user_id": owner, "provider": "paseo", "host_id": "other-host", "session_id": "another-physical-session", "daemon_id": "display-daemon"})
	legacy := externalTokenFixture(t, agent, owner, time.Now().Add(time.Hour))
	denied := serveAuthenticated(t, legacy, http.HandlerFunc(testHandler.GetBackgroundWaitScope), "GET", "/api/background-waits/scope?daemon_id=display-daemon", nil, nil, nil)
	if denied.Code != 403 {
		t.Fatal("unbound external credential got session scope")
	}
}

func TestWaitReceiptDaemonHostOwnerAndWorkspaceIsolation(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Read host scope", "scope-daemon")
	f := testutil.New(testPool, testWorkspaceID, owner)
	f.Runtime(t, "Scope host", testutil.Cols{"daemon_id": "scope-daemon"})
	req := protocol.WaitDeliveryRequest{Waits: []protocol.WaitLookup{{ID: uuid.NewString(), Address: protocol.WaitAddress{WorkspaceID: testWorkspaceID, AgentID: agent, SessionID: session, DaemonID: "scope-daemon"}}}}
	for _, tc := range []struct {
		name, workspace, daemon, owner string
		status                         int
	}{{"own host", testWorkspaceID, "scope-daemon", owner, 200}, {"other host", testWorkspaceID, "other-host", owner, 404}, {"other owner", testWorkspaceID, "scope-daemon", testUserID, 404}, {"other workspace", uuid.NewString(), "scope-daemon", owner, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDaemonTokenRequest("POST", "/api/daemon/background-waits/status", req, tc.workspace, tc.daemon)
			r = r.WithContext(middleware.WithDaemonTokenOwnerID(r.Context(), tc.owner))
			testutil.Call(t, testHandler.BackgroundWaitStatuses, r).Want(tc.status)
		})
	}
}
