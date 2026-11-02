package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// managedDispatcherFixture builds an active, session-pinned managed run and the
// mat_ credential the daemon would inject for it. The returned token is a real
// task token; requests in these tests run through the real Auth middleware.
func managedDispatcherFixture(t *testing.T, name string) (agentID, taskID, token string) {
	t.Helper()
	runtimeID := dbfx.Runtime(t, "Delegation Dispatcher Runtime "+name)
	agentID = dbfx.Agent(t, "Delegation Dispatcher "+name, runtimeID, testutil.Cols{
		"execution_kind":  "managed",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	scopeIssue := dbfx.Issue(t, "Delegation scope for "+name)
	taskID = dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":   scopeIssue,
		"runtime_id": runtimeID,
		"status":     "running",
		"session_id": "dispatcher-session-" + name,
		"work_dir":   "/work/dispatcher-" + name,
	})
	token = taskTokenFixture(t, agentID, taskID)
	return agentID, taskID, token
}

func taskTokenFixture(t *testing.T, agentID, taskID string) string {
	t.Helper()
	raw, err := auth.GenerateAgentTaskToken()
	if err != nil {
		t.Fatalf("generate task token: %v", err)
	}
	dbfx.Insert(t, "task_token", testutil.Cols{
		"token_hash":   auth.HashToken(raw),
		"task_id":      taskID,
		"agent_id":     agentID,
		"workspace_id": testWorkspaceID,
		"user_id":      testUserID,
		"expires_at":   time.Now().Add(time.Hour),
	})
	return raw
}

// executableTarget is a workspace-visible managed agent any member/agent may
// address.
func executableTarget(t *testing.T, name string) (agentID, runtimeID string) {
	t.Helper()
	runtimeID = dbfx.Runtime(t, "Delegation Target Runtime "+name)
	agentID = dbfx.Agent(t, "Delegation Target "+name, runtimeID, testutil.Cols{
		"execution_kind":  "managed",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	dbfx.Exec(t, `
		INSERT INTO agent_invocation_target (agent_id, target_type, target_id)
		VALUES ($1, 'workspace', $2)
		ON CONFLICT (agent_id, target_type, target_id) DO NOTHING
	`, agentID, testWorkspaceID)
	return agentID, runtimeID
}

// serveAuthenticated runs h inside the real Auth middleware with a bearer
// credential, returning the response. Extra headers are set on the inbound
// request (and therefore are subject to the middleware's stripping rules).
func serveAuthenticated(t *testing.T, token string, h http.Handler, method, path string, body any, params map[string]string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for key, value := range params {
			rctx.URLParams.Add(key, value)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	middleware.Auth(testHandler.Queries, nil, nil, nil)(h).ServeHTTP(w, req)
	return w
}

func createIssueBody(title string, assigneeID string) map[string]any {
	return map[string]any{
		"title":         title,
		"status":        "todo",
		"assignee_type": "agent",
		"assignee_id":   assigneeID,
	}
}

func issueIDByTitle(t *testing.T, title string) (string, bool) {
	t.Helper()
	var id string
	err := testPool.QueryRow(context.Background(),
		`SELECT id FROM issue WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, title).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

func acceptedReturn(t *testing.T, issueID string) (db.IssueDelegationReturn, bool) {
	t.Helper()
	rows := acceptedReturns(t, issueID)
	if len(rows) == 0 {
		return db.IssueDelegationReturn{}, false
	}
	return rows[0], true
}

func acceptedReturns(t *testing.T, issueID string) []db.IssueDelegationReturn {
	t.Helper()
	rows, err := testHandler.Queries.ListIssueDelegationReturns(context.Background(), db.ListIssueDelegationReturnsParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		IssueID:     parseUUID(issueID),
	})
	if err != nil {
		return nil
	}
	return rows
}

func taskReturnIDs(t *testing.T, taskID string) []db.IssueDelegationReturn {
	t.Helper()
	rows, err := testHandler.Queries.ListAgentTaskDelegationReturns(context.Background(), parseUUID(taskID))
	if err != nil {
		return nil
	}
	return rows
}

func latestTaskIDFor(t *testing.T, issueID, agentID string) string {
	t.Helper()
	var taskID string
	if err := testPool.QueryRow(context.Background(), `
		SELECT id FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2
		ORDER BY created_at DESC LIMIT 1`, issueID, agentID).Scan(&taskID); err != nil {
		t.Fatalf("latest task for %s/%s: %v", issueID, agentID, err)
	}
	return taskID
}

func returnIDs(rows []db.IssueDelegationReturn) map[string]bool {
	ids := make(map[string]bool, len(rows))
	for _, row := range rows {
		ids[uuidToString(row.ID)] = true
	}
	return ids
}

func requireTaskReturns(t *testing.T, taskID string, want ...db.IssueDelegationReturn) {
	t.Helper()
	got := returnIDs(taskReturnIDs(t, taskID))
	for _, row := range want {
		if !got[uuidToString(row.ID)] {
			t.Fatalf("task %s returns = %v, missing %s (dispatcher %s)", taskID, got, uuidToString(row.ID), uuidToString(row.DispatcherAgentID))
		}
	}
}

// bumpIssueCounter keeps the workspace counter ahead of numbers that fixture
// rows inserted directly. The handler allocates from the counter, so without
// this a create can collide with a fixture's MAX(number)+1.
func bumpIssueCounter(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `
		UPDATE workspace
		SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1))
		WHERE id = $1`, testWorkspaceID)
}

func countValue(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

func TestCreateDelegatedIssueByManagedDispatcherIsAtomic(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "atomic")
	target, targetRuntime := executableTarget(t, "atomic")
	title := fmt.Sprintf("managed delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatalf("issue %q was not created", title)
	}
	var creatorType, creatorID, originType, originID string
	if err := testPool.QueryRow(context.Background(), `
		SELECT creator_type, creator_id, COALESCE(origin_type,''), COALESCE(origin_id::text,'')
		FROM issue WHERE id = $1`, issueID).Scan(&creatorType, &creatorID, &originType, &originID); err != nil {
		t.Fatalf("load created issue: %v", err)
	}
	if creatorType != "agent" || creatorID != dispatcher {
		t.Fatalf("creator = %s/%s, want agent/%s", creatorType, creatorID, dispatcher)
	}
	if originType != "agent_create" || originID != sourceTask {
		t.Fatalf("origin = %s/%s, want agent_create/%s", originType, originID, sourceTask)
	}
	accepted, ok := acceptedReturn(t, issueID)
	if !ok {
		t.Fatal("delegated issue has no accepted return")
	}
	if accepted.ReturnKind != string(delegation.KindManagedTask) ||
		uuidToString(accepted.DispatcherAgentID) != dispatcher ||
		uuidToString(accepted.SourceTaskID) != sourceTask {
		t.Fatalf("accepted = %+v, want managed task %s for %s", accepted, sourceTask, dispatcher)
	}
	var taskID, delegatedFrom, taskStatus, runtimeID string
	if err := testPool.QueryRow(context.Background(), `
		SELECT id, COALESCE(delegated_from_task_id::text,''), status, runtime_id
		FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, target).Scan(&taskID, &delegatedFrom, &taskStatus, &runtimeID); err != nil {
		t.Fatalf("delegated task: %v", err)
	}
	requireTaskReturns(t, taskID, accepted)
	if delegatedFrom != sourceTask {
		t.Fatalf("task delegated_from = %q, want source %s", delegatedFrom, sourceTask)
	}
	if taskStatus != "queued" || runtimeID != targetRuntime {
		t.Fatalf("task = %s on %s, want queued on %s", taskStatus, runtimeID, targetRuntime)
	}
}

func TestCreateDelegatedIssueNotReadyCreatesNothing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Delegation Unpinned Runtime")
	dispatcher := dbfx.Agent(t, "Delegation Unpinned Dispatcher", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Delegation unpinned scope")
	sourceTask := dbfx.Task(t, dispatcher, testutil.Cols{
		"issue_id": scope, "runtime_id": runtimeID, "status": "running", "work_dir": "/work/unpinned",
	})
	token := taskTokenFixture(t, dispatcher, sourceTask)
	target, _ := executableTarget(t, "unpinned")
	title := fmt.Sprintf("not ready delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&payload)
	if payload.Code != "delegation_return_not_ready" {
		t.Fatalf("code = %q, want delegation_return_not_ready", payload.Code)
	}
	if _, ok := issueIDByTitle(t, title); ok {
		t.Fatal("not-ready delegation created an issue")
	}
}

func TestCreateDelegatedIssueByExternalDispatcherBindsExplicitSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sessionID, _ := externalAgentFixture(t, "Delegation External", "delegation-daemon")
	ownerID := ""
	_ = testPool.QueryRow(context.Background(), `SELECT owner_user_id FROM external_agent_session WHERE id = $1`, sessionID).Scan(&ownerID)
	hostSession := ""
	_ = testPool.QueryRow(context.Background(), `SELECT session_id FROM external_agent_session WHERE id = $1`, sessionID).Scan(&hostSession)
	token := externalTokenFixture(t, dispatcher, ownerID, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "external")
	title := fmt.Sprintf("external delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, func(r *http.Request) {
		r.Header.Set(ReturnSessionHeader, hostSession)
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("delegated issue was not created")
	}
	accepted, ok := acceptedReturn(t, issueID)
	if !ok {
		t.Fatal("external delegation has no accepted return")
	}
	if accepted.ReturnKind != string(delegation.KindExternalSession) ||
		uuidToString(accepted.ExternalSessionID) != sessionID ||
		uuidToString(accepted.DispatcherAgentID) != dispatcher {
		t.Fatalf("accepted = %+v, want external session %s for %s", accepted, sessionID, dispatcher)
	}
	binding, err := sessionroute.Resolve(context.Background(), testHandler.Queries, testWorkspaceID, dispatcher, issueID)
	if err != nil {
		t.Fatalf("resolve external binding: %v", err)
	}
	if binding.SessionID != sessionID {
		t.Fatalf("binding session = %s, want %s", binding.SessionID, sessionID)
	}
	requireTaskReturns(t, latestTaskIDFor(t, issueID, target), accepted)
}

func TestCreateDelegatedIssueByExternalDispatcherWithoutSessionCreatesNothing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sessionID, _ := externalAgentFixture(t, "Delegation External Missing", "delegation-daemon")
	ownerID := ""
	_ = testPool.QueryRow(context.Background(), `SELECT owner_user_id FROM external_agent_session WHERE id = $1`, sessionID).Scan(&ownerID)
	token := externalTokenFixture(t, dispatcher, ownerID, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "external-missing")
	title := fmt.Sprintf("external missing delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if _, ok := issueIDByTitle(t, title); ok {
		t.Fatal("missing-address delegation created an issue")
	}
}

func TestCreateDelegatedIssueByExternalDispatcherRejectsForeignSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sessionID, _ := externalAgentFixture(t, "Delegation External Foreign", "delegation-daemon")
	ownerID := ""
	_ = testPool.QueryRow(context.Background(), `SELECT owner_user_id FROM external_agent_session WHERE id = $1`, sessionID).Scan(&ownerID)
	token := externalTokenFixture(t, dispatcher, ownerID, time.Now().Add(time.Hour))
	_, otherSession, _ := externalAgentFixture(t, "Delegation External Other", "delegation-daemon")
	target, _ := executableTarget(t, "external-foreign")
	title := fmt.Sprintf("external foreign delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, func(r *http.Request) {
		r.Header.Set(ReturnSessionHeader, otherSession)
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if _, ok := issueIDByTitle(t, title); ok {
		t.Fatal("foreign-address delegation created an issue")
	}
}

func TestCreateIssueByMemberWithAgentAssigneeIsUnchanged(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	target, _ := executableTarget(t, "member-create")
	title := fmt.Sprintf("member delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues", createIssueBody(title, target))
	testHandler.CreateIssue(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("member issue was not created")
	}
	if _, ok := acceptedReturn(t, issueID); ok {
		t.Fatal("member create recorded a delegation return it does not need")
	}
}

func TestCreateDelegatedIssueRollsBackWhenReturnWriteFails(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, _, token := managedDispatcherFixture(t, "return-failure")
	target, _ := executableTarget(t, "return-failure")
	fn := "work86_fail_return_" + strings.ReplaceAll(dispatcher, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.dispatcher_agent_id = '%s' THEN
				RAISE EXCEPTION 'injected return write failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON issue_delegation_return
		FOR EACH ROW EXECUTE FUNCTION %s();
	`, fn, dispatcher, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON issue_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	title := fmt.Sprintf("return failure delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if _, ok := issueIDByTitle(t, title); ok {
		t.Fatal("return-write failure left the delegated issue committed")
	}
}

func TestCreateDelegatedIssueRollsBackWhenTaskWriteFails(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, token := managedDispatcherFixture(t, "task-failure")
	target, _ := executableTarget(t, "task-failure")
	fn := "work86_fail_task_" + strings.ReplaceAll(target, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.agent_id = '%s' THEN
				RAISE EXCEPTION 'injected task write failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue
		FOR EACH ROW EXECUTE FUNCTION %s();
	`, fn, target, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	title := fmt.Sprintf("task failure delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if _, ok := issueIDByTitle(t, title); ok {
		t.Fatal("task-write failure left the delegated issue committed")
	}
}

func TestUpdateAssignmentByManagedDispatcherRecordsReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "assign")
	target, _ := executableTarget(t, "assign")
	issueID := dbfx.Issue(t, "Assign via managed dispatcher")

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent",
		"assignee_id":   target,
	}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	accepted, ok := acceptedReturn(t, issueID)
	if !ok {
		t.Fatal("assignment by a managed dispatcher recorded no return")
	}
	if accepted.ReturnKind != string(delegation.KindManagedTask) ||
		uuidToString(accepted.SourceTaskID) != sourceTask ||
		uuidToString(accepted.DispatcherAgentID) != dispatcher {
		t.Fatalf("accepted = %+v, want managed task %s", accepted, sourceTask)
	}
	requireTaskReturns(t, latestTaskIDFor(t, issueID, target), accepted)
}

func TestUpdateAssignmentByUnpinnedManagedDispatcherChangesNothing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Delegation Assign Unpinned Runtime")
	dispatcher := dbfx.Agent(t, "Delegation Assign Unpinned", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Delegation assign unpinned scope")
	sourceTask := dbfx.Task(t, dispatcher, testutil.Cols{"issue_id": scope, "runtime_id": runtimeID, "status": "running"})
	token := taskTokenFixture(t, dispatcher, sourceTask)
	target, _ := executableTarget(t, "assign-unpinned")
	issueID := dbfx.Issue(t, "Assign via unpinned dispatcher")

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent",
		"assignee_id":   target,
	}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var assignee string
	if err := testPool.QueryRow(context.Background(), `SELECT COALESCE(assignee_id::text,'') FROM issue WHERE id = $1`, issueID).Scan(&assignee); err != nil {
		t.Fatalf("load issue: %v", err)
	}
	if assignee != "" {
		t.Fatalf("refused assignment still changed assignee to %s", assignee)
	}
}

func TestAgentMentionCoalescingPreservesEveryDispatcher(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	firstAgent, firstTask, firstToken := managedDispatcherFixture(t, "mention-first")
	secondAgent, secondTask, secondToken := managedDispatcherFixture(t, "mention-second")
	target, _ := executableTarget(t, "mention-target")
	issueID := dbfx.Issue(t, "Mention coalescing")
	post := func(token, content string, parentID string) *httptest.ResponseRecorder {
		body := map[string]any{"content": content}
		if parentID != "" {
			body["parent_id"] = parentID
		}
		return serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", body, map[string]string{"id": issueID}, nil)
	}

	firstResponse := post(firstToken, fmt.Sprintf("[@Target](mention://agent/%s) first request %d", target, time.Now().UnixNano()), "")
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first mention status = %d: %s", firstResponse.Code, firstResponse.Body.String())
	}
	var firstComment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(firstResponse.Body.Bytes(), &firstComment); err != nil {
		t.Fatalf("decode first comment: %v", err)
	}
	firstRows := acceptedReturns(t, issueID)
	if len(firstRows) != 1 || uuidToString(firstRows[0].DispatcherAgentID) != firstAgent || uuidToString(firstRows[0].SourceTaskID) != firstTask {
		t.Fatalf("first dispatcher returns = %+v, want one managed row for %s", firstRows, firstAgent)
	}
	targetTaskID := latestTaskIDFor(t, issueID, target)
	requireTaskReturns(t, targetTaskID, firstRows[0])

	// The second agent explicitly mentions the SAME worker. The request folds
	// into the pending run; the run must keep both accepted relationships, not
	// just the first one.
	// Same thread: the second explicit mention folds into the pending run.
	if w := post(secondToken, fmt.Sprintf("[@Target](mention://agent/%s) second request %d", target, time.Now().UnixNano()), firstComment.ID); w.Code != http.StatusCreated {
		t.Fatalf("second mention status = %d: %s", w.Code, w.Body.String())
	}
	rows := acceptedReturns(t, issueID)
	if len(rows) != 2 {
		t.Fatalf("accepted returns = %d, want one per dispatcher: %+v", len(rows), rows)
	}
	byDispatcher := map[string]db.IssueDelegationReturn{}
	for _, row := range rows {
		byDispatcher[uuidToString(row.DispatcherAgentID)] = row
	}
	if _, ok := byDispatcher[firstAgent]; !ok {
		t.Fatalf("coalescing dropped the first dispatcher's return: %+v", rows)
	}
	secondReturn, ok := byDispatcher[secondAgent]
	if !ok {
		t.Fatalf("coalescing dropped the second dispatcher's return: %+v", rows)
	}
	if uuidToString(secondReturn.SourceTaskID) != secondTask {
		t.Fatalf("second return source = %s, want %s", uuidToString(secondReturn.SourceTaskID), secondTask)
	}
	if latestTaskIDFor(t, issueID, target) != targetTaskID {
		t.Fatal("second mention created a second run instead of coalescing")
	}
	requireTaskReturns(t, targetTaskID, rows...)
}

// A second dispatcher's INDEPENDENT delegation (different worker, so no
// coalescing) must be addressed to that dispatcher, not to the first one.
func TestSecondDispatcherIndependentDelegationGetsItsOwnReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	firstAgent, firstTask, firstToken := managedDispatcherFixture(t, "two-dispatchers-first")
	secondAgent, secondTask, secondToken := managedDispatcherFixture(t, "two-dispatchers-second")
	targetA, _ := executableTarget(t, "two-dispatchers-a")
	targetB, _ := executableTarget(t, "two-dispatchers-b")
	issueID := dbfx.Issue(t, "Two dispatchers, two workers")
	post := func(token, content string) *httptest.ResponseRecorder {
		return serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
			"content": content,
		}, map[string]string{"id": issueID}, nil)
	}

	if w := post(firstToken, fmt.Sprintf("[@A](mention://agent/%s) first delegation %d", targetA, time.Now().UnixNano())); w.Code != http.StatusCreated {
		t.Fatalf("first mention = %d: %s", w.Code, w.Body.String())
	}
	if w := post(secondToken, fmt.Sprintf("[@B](mention://agent/%s) second delegation %d", targetB, time.Now().UnixNano())); w.Code != http.StatusCreated {
		t.Fatalf("second mention = %d: %s", w.Code, w.Body.String())
	}
	rows := acceptedReturns(t, issueID)
	if len(rows) != 2 {
		t.Fatalf("accepted returns = %d, want 2: %+v", len(rows), rows)
	}
	taskA := latestTaskIDFor(t, issueID, targetA)
	taskB := latestTaskIDFor(t, issueID, targetB)
	returnsA := taskReturnIDs(t, taskA)
	returnsB := taskReturnIDs(t, taskB)
	if len(returnsA) != 1 || uuidToString(returnsA[0].DispatcherAgentID) != firstAgent || uuidToString(returnsA[0].SourceTaskID) != firstTask {
		t.Fatalf("worker A returns = %+v, want only %s/%s", returnsA, firstAgent, firstTask)
	}
	if len(returnsB) != 1 || uuidToString(returnsB[0].DispatcherAgentID) != secondAgent || uuidToString(returnsB[0].SourceTaskID) != secondTask {
		t.Fatalf("worker B returns = %+v, want only %s/%s (not the first dispatcher %s)", returnsB, secondAgent, secondTask, firstAgent)
	}
}

func TestAgentMentionWithoutPinnedSessionCreatesNoComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Delegation Mention Unpinned Runtime")
	agent := dbfx.Agent(t, "Delegation Mention Unpinned", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Delegation mention unpinned scope")
	task := dbfx.Task(t, agent, testutil.Cols{"issue_id": scope, "runtime_id": runtimeID, "status": "running"})
	token := taskTokenFixture(t, agent, task)
	target, _ := executableTarget(t, "mention-unpinned")
	issueID := dbfx.Issue(t, "Mention unpinned delegation")
	content := fmt.Sprintf("[@Target](mention://agent/%s) no session yet %d", target, time.Now().UnixNano())

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": content,
	}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = $2`, issueID, content); n != 0 {
		t.Fatalf("refused mention saved %d comments", n)
	}
}

func TestBacklogDelegationRecordsReturnWithoutRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, token := managedDispatcherFixture(t, "backlog")
	target, _ := executableTarget(t, "backlog")
	title := fmt.Sprintf("backlog delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	body := createIssueBody(title, target)
	body["status"] = "backlog"

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", body, nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("backlog delegation was not created")
	}
	if _, ok := acceptedReturn(t, issueID); !ok {
		t.Fatal("backlog delegation did not record its return address")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("backlog delegation started %d runs", n)
	}
}

func TestReassignmentPreservesAcceptedReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	externalAgent, externalSessionID, _ := externalAgentFixture(t, "Reassign External", "delegation-daemon")
	_, sourceTask, token := managedDispatcherFixture(t, "reassign")
	firstTarget, _ := executableTarget(t, "reassign-first")
	secondTarget, _ := executableTarget(t, "reassign-second")
	issueID := dbfx.Issue(t, "Reassign delegated issue")
	accepted, err := delegation.Ensure(context.Background(), testHandler.Queries, testWorkspaceID, issueID, delegation.Address{
		DispatcherAgentID: externalAgent, Kind: delegation.KindExternalSession, ExternalSessionID: externalSessionID,
	}, testUserID)
	if err != nil {
		t.Fatalf("seed accepted return: %v", err)
	}
	// Give the issue a current assignee so the update is a real reassignment.
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, firstTarget, issueID)
	_ = sourceTask

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent",
		"assignee_id":   secondTarget,
	}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	still, ok := acceptedReturn(t, issueID)
	if !ok || still.ID != accepted.ID || uuidToString(still.DispatcherAgentID) != externalAgent {
		t.Fatalf("reassignment changed the accepted return: %+v", still)
	}
	requireTaskReturns(t, latestTaskIDFor(t, issueID, secondTarget), accepted)
}

func TestRerunByManagedDispatcherPreservesAcceptedReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	externalAgent, externalSessionID, _ := externalAgentFixture(t, "Rerun External", "delegation-daemon")
	dispatcher, _, token := managedDispatcherFixture(t, "rerun")
	target, runtimeID := executableTarget(t, "rerun")
	issueID := dbfx.Issue(t, "Rerun delegated issue", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   target,
		"status":        "in_progress",
	})
	dbfx.Task(t, target, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "completed",
		"session_id": "target-old-session", "work_dir": "/work/target",
	})
	accepted, err := delegation.Ensure(context.Background(), testHandler.Queries, testWorkspaceID, issueID, delegation.Address{
		DispatcherAgentID: externalAgent, Kind: delegation.KindExternalSession, ExternalSessionID: externalSessionID,
	}, testUserID)
	if err != nil {
		t.Fatalf("seed accepted return: %v", err)
	}
	_ = dispatcher

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost, "/api/issues/"+issueID+"/rerun", map[string]any{}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	still, ok := acceptedReturn(t, issueID)
	if !ok || still.ID != accepted.ID || uuidToString(still.DispatcherAgentID) != externalAgent {
		t.Fatalf("rerun changed the accepted return: %+v", still)
	}
	rerunTaskID := latestTaskIDFor(t, issueID, target)
	requireTaskReturns(t, rerunTaskID, accepted)
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, target); n != 2 {
		t.Fatalf("rerun created %d tasks for the target, want the completed source plus the rerun", n)
	}
}
