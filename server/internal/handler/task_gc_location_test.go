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

func gcLocationRequest(t *testing.T, token, taskID, method, path string, body any, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := withURLParam(httptest.NewRequest(method, path, bytes.NewReader(data)), "taskId", taskID)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Workspace-ID", testWorkspaceID)
	response := httptest.NewRecorder()
	middleware.DaemonAuth(testHandler.Queries, auth.NewPATCache(nil), auth.NewDaemonTokenCache(nil), nil)(handler).ServeHTTP(response, request)
	return response
}

func TestTaskGCCheckProjectsOnlyPersistedReleasedLocation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	runtime := dbfx.Runtime(t, "GC location host", testutil.Cols{"provider": "codex", "runtime_mode": "local", "daemon_id": "gc-location-host"})
	agent := dbfx.Agent(t, "GC location owner", runtime)
	contextJSON, _ := json.Marshal(map[string]string{"type": "quick_create", "workspace_id": testWorkspaceID, "requester_id": testUserID, "prompt": "fixture"})
	task := dbfx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "completed", "completed_at": testutil.Raw("now()"), "resource_control_required": true, "terminal_reported_at": testutil.Raw("now()"), "context": contextJSON, "session_id": "gc-session", "work_dir": "/owned/task/worktree/subdir", "result": []byte(`{"output":"saved result","durable_work_dir":"/untrusted/result-json-location"}`)})
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	before, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(task))
	if err != nil {
		t.Fatal(err)
	}
	response := gcLocationRequest(t, token, task, "GET", "/gc-check", nil, testHandler.GetTaskGCCheck)
	if response.Code != 200 {
		t.Fatalf("GET HTTP%d %s", response.Code, response.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	value, present := fields["durable_work_dir"]
	if !present || string(value) != "null" {
		t.Fatalf("unreported cleanup must be explicit null, not inferred from terminal/result JSON: %s", response.Body.String())
	}
	receipt := protocol.ResourceReleaseReceipt{ResourceReleaseRequest: protocol.ResourceReleaseRequest{Kind: "workdir", SourceTaskID: task, RuntimeID: runtime, Provider: "codex", WorkDir: before.WorkDir.String, ResourcePath: "/owned/task", DaemonID: "gc-location-host"}, DurableWorkDir: "/user/repository/subdir"}
	response = gcLocationRequest(t, token, task, "POST", "/resources-released", receipt, testHandler.RecordResourcesReleased)
	if response.Code != 200 {
		t.Fatalf("ACK HTTP%d %s", response.Code, response.Body.String())
	}
	response = gcLocationRequest(t, token, task, "GET", "/gc-check", nil, testHandler.GetTaskGCCheck)
	if response.Code != 200 {
		t.Fatalf("GET after ACK HTTP%d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	var durable string
	if err := json.Unmarshal(fields["durable_work_dir"], &durable); err != nil || durable != receipt.DurableWorkDir {
		t.Fatalf("persisted ACK location missing: %s (%v)", response.Body.String(), err)
	}
	if string(fields["resource_control_required"]) != "true" || string(fields["terminal_reported_at"]) == "null" {
		t.Fatal("location projection dropped existing resource observations")
	}
	after, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(task))
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Result) != string(before.Result) || after.WorkDir != before.WorkDir {
		t.Fatal("read/ACK rewrote original result or working address")
	}
	foreign := dbfx.User(t, "GC foreign owner", "gc-foreign-location@example.com")
	dbfx.Member(t, testWorkspaceID, foreign, "member")
	foreignToken, patID := insertTestPAT(t, time.Now().Add(time.Hour))
	dbfx.Exec(t, `UPDATE personal_access_token SET user_id=$2 WHERE id=$1`, patID, foreign)
	response = gcLocationRequest(t, foreignToken, task, "GET", "/gc-check", nil, testHandler.GetTaskGCCheck)
	if response.Code != 404 {
		t.Fatalf("foreign runtime owner read cleanup location: HTTP%d", response.Code)
	}
}
