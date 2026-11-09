package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestBackgroundWaitCompletionAuthorityAndIdempotency(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Background wait", "wait-daemon")
	f := testutil.New(testPool, testWorkspaceID, owner)
	f.Runtime(t, "Wait daemon", testutil.Cols{"daemon_id": "wait-daemon"})
	result := protocol.WaitResult{ID: uuid.NewString(), Address: protocol.WaitAddress{WorkspaceID: testWorkspaceID, AgentID: agent, SessionID: session, DaemonID: "wait-daemon"}, State: "succeeded", FinishedAt: time.Now().UTC(), LogPath: "/private/wait.log"}
	request := func(value protocol.WaitResult, ws, daemon, tokenOwner string) *http.Request {
		r := newDaemonTokenRequest(http.MethodPost, "/api/daemon/background-waits/complete", value, ws, daemon)
		ctx := middleware.WithDaemonTokenOwnerID(r.Context(), tokenOwner)
		return r.WithContext(ctx)
	}
	for _, tc := range []struct{ name, workspace, daemon, owner string }{
		{"wrong daemon", testWorkspaceID, "other-daemon", owner},
		{"wrong owner", testWorkspaceID, "wait-daemon", testUserID},
		{"wrong workspace", uuid.NewString(), "wait-daemon", owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, testHandler.CompleteBackgroundWait, request(result, tc.workspace, tc.daemon, tc.owner)).Want(http.StatusNotFound)
		})
	}
	var first, second struct {
		ID string `json:"delivery_id"`
	}
	testutil.Call(t, testHandler.CompleteBackgroundWait, request(result, testWorkspaceID, "wait-daemon", owner)).Want(http.StatusOK).JSON(&first)
	dbfx.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE id=$1", first.ID)
	testutil.Call(t, testHandler.CompleteBackgroundWait, request(result, testWorkspaceID, "wait-daemon", owner)).Want(http.StatusOK).JSON(&second)
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("unstable delivery: %+v %+v", first, second)
	}
	changed := result
	changed.State = "failed"
	testutil.Call(t, testHandler.CompleteBackgroundWait, request(changed, testWorkspaceID, "wait-daemon", owner)).Want(http.StatusConflict)
	var count int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM agent_mailbox_message WHERE source_type='background_wait' AND source_key=$1", "background_wait:"+result.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate completion rows=%d err=%v", count, err)
	}
}

func TestBackgroundWaitRegistrationAuthentication(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Wait registration", "registration-daemon")
	f := testutil.New(testPool, testWorkspaceID, owner)
	f.Runtime(t, "Registration daemon", testutil.Cols{"daemon_id": "registration-daemon"})
	token := externalTokenFixture(t, agent, owner, time.Now().Add(time.Hour))
	call := func(raw, daemon, sessionRef, workspace string) int {
		return serveAuthenticated(t, raw, http.HandlerFunc(testHandler.AuthorizeBackgroundWait), "POST", "/api/background-waits/authorize", map[string]string{"daemon_id": daemon}, nil, func(r *http.Request) {
			r.Header.Set(ReturnSessionHeader, sessionRef)
			r.Header.Set("X-Workspace-ID", workspace)
			// Neither header may replace the credential's authenticated identity.
			r.Header.Set("X-Actor-Source", "external_agent")
			r.Header.Set("X-Agent-ID", agent)
		}).Code
	}
	if got := call(token, "registration-daemon", session, testWorkspaceID); got != 200 {
		t.Fatalf("valid authorization: %d", got)
	}
	if got := call(token, "wrong-daemon", session, testWorkspaceID); got != 403 {
		t.Fatalf("wrong daemon: %d", got)
	}
	if got := call(token, "registration-daemon", uuid.NewString(), testWorkspaceID); got != 403 {
		t.Fatalf("unregistered session: %d", got)
	}
	if got := call(token, "registration-daemon", session, uuid.NewString()); got != 403 {
		t.Fatalf("cross-workspace: %d", got)
	}
	managed := dbfx.Agent(t, "Managed wait requester", testRuntimeID)
	task := dbfx.Task(t, managed, testutil.Cols{"status": "running", "runtime_id": testRuntimeID})
	if got := call(taskTokenFixture(t, managed, task), "registration-daemon", session, testWorkspaceID); got < 400 {
		t.Fatalf("managed credential selected external session: %d", got)
	}
	dbfx.Exec(t, "UPDATE agent SET archived_at=now() WHERE id=$1", agent)
	if got := call(token, "registration-daemon", session, testWorkspaceID); got != 401 {
		t.Fatalf("archived external identity: %d", got)
	}
}
