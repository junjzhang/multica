package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func authenticatedResourceRelease(t *testing.T, token string, body protocol.ResourceReleaseRequest) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/resources/release", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	out := httptest.NewRecorder()
	middleware.DaemonAuth(testHandler.Queries, auth.NewPATCache(nil), auth.NewDaemonTokenCache(nil), nil)(http.HandlerFunc(testHandler.ReleaseManagedResource)).ServeHTTP(out, req)
	return out
}

func TestLegacyProviderStoreReleaseWithPAT(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	runtimeID := dbfx.Runtime(t, "legacy release host", testutil.Cols{"runtime_mode": "local", "provider": "codex", "daemon_id": "legacy-release-host"})
	agentID := dbfx.Agent(t, "legacy provider store", runtimeID)
	issueID := dbfx.Issue(t, "legacy provider scope")
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	legacy := protocol.ResourceReleaseRequest{Kind: "provider_store", Provider: "codex", AgentID: agentID, ScopeType: "issue", ScopeID: issueID, ResourcePath: "/owned/legacy/provider-store"}
	before := authenticatedResourceRelease(t, token, legacy)
	t.Logf("legacy request without daemon proof: HTTP %d", before.Code)
	if before.Code == http.StatusOK {
		t.Fatal("unproven daemon identity was guessed from agent current runtime")
	}
	request := legacy
	request.DaemonID = "legacy-release-host"
	response := authenticatedResourceRelease(t, token, request)
	if response.Code != http.StatusOK {
		t.Fatalf("owned legacy store lost TTL release: HTTP %d %s", response.Code, response.Body.String())
	}
	var decision protocol.ResourceReleaseDecision
	if err := json.Unmarshal(response.Body.Bytes(), &decision); err != nil {
		t.Fatal(err)
	}
	if !decision.ReleaseAllowed || decision.ControlRequired {
		t.Fatalf("legacy no-dependency release: %+v", decision)
	}

	foreign := dbfx.User(t, "foreign daemon owner", "foreign-release-owner@example.com")
	dbfx.Member(t, testWorkspaceID, foreign, "member")
	foreignRuntime := dbfx.Runtime(t, "foreign release host", testutil.Cols{"provider": "codex", "daemon_id": "foreign-release-host", "owner_id": foreign})
	foreignAgent := dbfx.Agent(t, "foreign hosted resource", foreignRuntime, testutil.Cols{"owner_id": foreign})
	foreignActorRequest := request
	foreignActorRequest.AgentID = foreignAgent
	if response := authenticatedResourceRelease(t, token, foreignActorRequest); response.Code == http.StatusOK {
		t.Fatal("unhosted foreign actor was accepted from path alone")
	}
	denied := request
	denied.DaemonID = "foreign-release-host"
	if response := authenticatedResourceRelease(t, token, denied); response.Code == http.StatusOK {
		t.Fatal("PAT acted as another owner's daemon")
	}
	otherWS := dbfx.Workspace(t, "foreign resource workspace", "foreign-resource-release")
	otherAgent := dbfx.Agent(t, "foreign workspace agent", "", testutil.Cols{"workspace_id": otherWS, "owner_id": foreign})
	denied = request
	denied.AgentID = otherAgent
	if response := authenticatedResourceRelease(t, token, denied); response.Code == http.StatusOK {
		t.Fatal("default workspace header authorized foreign actual workspace")
	}
	denied = request
	denied.WorkspaceID = otherWS
	if response := authenticatedResourceRelease(t, token, denied); response.Code == http.StatusOK {
		t.Fatal("explicit workspace conflict was ignored")
	}
	denied = request
	denied.DaemonID = "foreign-release-host"
	testutil.Call(t, testHandler.ReleaseManagedResource, newDaemonTokenRequest(http.MethodPost, "/release", denied, testWorkspaceID, "legacy-release-host")).Want(http.StatusConflict)
	otherHost := dbfx.Runtime(t, "agent moved to another host", testutil.Cols{"provider": "codex", "daemon_id": "new-release-host"})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "completed"})
	mismatched := request
	mismatched.RuntimeID = runtimeID
	mismatched.DaemonID = "new-release-host"
	if response := authenticatedResourceRelease(t, token, mismatched); response.Code != http.StatusConflict {
		t.Fatalf("declared daemon did not match runtime: %d", response.Code)
	}
	dbfx.Exec(t, `UPDATE agent SET runtime_id=$2 WHERE id=$1`, agentID, otherHost)
	response = authenticatedResourceRelease(t, token, request)
	if response.Code != 200 {
		t.Fatalf("historical owned host was replaced by agent latest runtime: %d %s", response.Code, response.Body.String())
	}

}

func TestHistoricalDirectoryReleaseAndIdempotentOwnerReceipt(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, _ := resultFixture(t, "historical-release")
	oldDir := source.WorkDir.String
	dbfx.Exec(t, `UPDATE agent_task_queue SET session_id='current-B',work_dir='/work/current-B' WHERE id=$1`, source.ID)
	request := protocol.ResourceReleaseRequest{Kind: "workdir", WorkspaceID: testWorkspaceID, SourceTaskID: uuidToString(source.ID), RuntimeID: uuidToString(source.RuntimeID), Provider: "codex", WorkDir: oldDir, ResourcePath: oldDir}
	var before protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", request)).Want(200).JSON(&before)
	if before.ReleaseAllowed {
		t.Fatal("current B hid the accepted A dependency")
	}
	finishResultFixture(t, result)
	completeResultHandlingTurn(t, result)
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, result.IssueID)
	var allowed protocol.ResourceReleaseDecision
	testutil.Call(t, testHandler.ReleaseManagedResource, newOwnerDaemonRequest("POST", "/release", request)).Want(200).JSON(&allowed)
	if !allowed.ReleaseAllowed {
		t.Fatalf("historical A permanently rejected because current source is B: %+v", allowed)
	}
	receipt := protocol.ResourceReleaseReceipt{ResourceReleaseRequest: request, DurableWorkDir: "/repo/stable"}
	var stale protocol.ResourceReleaseAcknowledgement
	testutil.Call(t, testHandler.RecordResourcesReleased, withURLParam(newOwnerDaemonRequest("POST", "/resources-released", receipt), "taskId", uuidToString(source.ID))).Want(200).JSON(&stale)
	if stale.Recorded {
		t.Fatal("removing historical A redirected current B")
	}
	var ownerID string
	dbfx.QueryRow(t, `SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1`, result.ID).Scan(&ownerID)
	testutil.Call(t, testHandler.RecordResourcesReleased, withURLParam(newOwnerDaemonRequest("POST", "/resources-released", receipt), "taskId", ownerID)).Want(200)
	active := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"runtime_id": uuidToString(source.RuntimeID), "issue_id": uuidToString(source.IssueID), "status": "running", "work_dir": oldDir, "resource_control_required": true})
	// A replay only acknowledges the already recorded deletion; it grants no
	// permission to remove a later run's recreated directory.
	var replay protocol.ResourceReleaseAcknowledgement
	testutil.Call(t, testHandler.RecordResourcesReleased, withURLParam(newOwnerDaemonRequest("POST", "/resources-released", receipt), "taskId", ownerID)).Want(200).JSON(&replay)
	if !replay.Recorded || taskStatusOf(t, active) != "running" {
		t.Fatal("receipt replay was not idempotent")
	}
	current, err := testHandler.Queries.GetAgentTask(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.WorkDir.String != "/work/current-B" || current.DurableWorkDir.Valid {
		t.Fatal("receiver receipt mutated original source B")
	}
}
