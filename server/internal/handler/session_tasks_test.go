package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func sessionTasksRequest(userID, agentID string, external bool, sessionID string) *http.Request {
	req := newRequestAs(userID, http.MethodGet, "/api/agents/"+agentID+"/session-tasks", nil)
	if !external {
		return withURLParam(req, "id", agentID)
	}
	req.Header.Set("X-Actor-Source", "external_agent")
	req.Header.Set("X-Agent-ID", agentID)
	if sessionID != "" {
		req.Header.Set(ActorSessionHeader, sessionID)
	}
	return withURLParam(req, "id", agentID)
}

func getSessionTasks(t *testing.T, req *http.Request) (int, SessionTasksResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.GetAgentSessionTasks(w, req)
	var resp SessionTasksResponse
	if w.Code != http.StatusOK {
		t.Logf("session tasks response %d: %s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode session tasks: %v", err)
		}
	}
	return w.Code, resp
}

func sessionTaskRoles(items []SessionTaskItem) map[string]string {
	roles := make(map[string]string, len(items))
	for _, item := range items {
		roles[item.IssueID] = item.Role
	}
	return roles
}

func testContractActor(id string) map[string]any {
	return map[string]any{"type": "agent", "id": id}
}

func contractVersion(number int, acceptorID, executorID string) map[string]any {
	return map[string]any{
		"number":             number,
		"goal":               "deliver",
		"scope":              "slice",
		"authorization_note": "",
		"criteria":           []string{"done"},
		"executor":           testContractActor(executorID),
		"acceptor":           testContractActor(acceptorID),
		"author":             testContractActor(acceptorID),
		"created_at":         time.Now().UTC().Format(time.RFC3339),
	}
}

func insertIssueContract(t *testing.T, issueID string, versions []map[string]any, deliveries, decisions []map[string]any) {
	t.Helper()
	state := map[string]any{
		"sequence":   1,
		"versions":   versions,
		"deliveries": deliveries,
		"decisions":  decisions,
		"requests":   []any{},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode contract: %v", err)
	}
	dbfx.Exec(t, `INSERT INTO issue_contract (issue_id, workspace_id, data) VALUES ($1, $2, $3::jsonb)`,
		issueID, testWorkspaceID, string(encoded))
}

func pendingDelivery(version int, authorID string) map[string]any {
	return map[string]any{
		"id":         "delivery-1",
		"version":    version,
		"criteria":   []int{0},
		"evidence":   "evidence",
		"child_ids":  []string{},
		"author":     testContractActor(authorID),
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
}

func decidedDelivery(version int, authorID string) ([]map[string]any, []map[string]any) {
	delivery := pendingDelivery(version, authorID)
	decision := map[string]any{
		"delivery_id": "delivery-1",
		"version":     version,
		"outcome":     "accept",
		"criteria":    []int{0},
		"reason":      "",
		"child_ids":   []string{},
		"author":      testContractActor(authorID),
		"created_at":  time.Now().UTC().Format(time.RFC3339),
	}
	return []map[string]any{delivery}, []map[string]any{decision}
}

// TestGetAgentSessionTasksReadAPI pins the session panel's three lists, the
// role of each item, the excluded states, and the credential scoping. It is a
// DB-backed read: the contract and delegation shapes only exist in Postgres.
func TestGetAgentSessionTasksReadAPI(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	previousAppURL := testHandler.cfg.AppURL
	testHandler.cfg.AppURL = "https://app.example.test"
	t.Cleanup(func() { testHandler.cfg.AppURL = previousAppURL })

	agentID, sessionID, ownerID := externalAgentFixture(t, "Session Tasks", "daemon-session-tasks")
	dispatcherID, dispatcherSession, _ := externalAgentFixture(t, "Session Tasks Dispatcher", "daemon-session-tasks-dispatcher")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// Owned as executor.
	executorIssue := dbfx.Issue(t, "session tasks executor "+suffix, testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
		"status":        "in_progress",
	})
	// Owned as task leader of a root and its child.
	rootIssue := dbfx.Issue(t, "session tasks root "+suffix)
	dbfx.Exec(t, "UPDATE issue SET leadership_root_issue_id = id, leader_agent_id = $1 WHERE id = $2", agentID, rootIssue)
	childIssue := dbfx.Issue(t, "session tasks child "+suffix, testutil.Cols{"parent_issue_id": rootIssue})
	dbfx.Exec(t, "UPDATE issue SET leadership_root_issue_id = $1 WHERE id = $2", rootIssue, childIssue)
	// Excluded: finished work and another agent's issue.
	dbfx.Issue(t, "session tasks done "+suffix, testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
		"status":        "done",
	})
	foreignIssue := dbfx.Issue(t, "session tasks foreign "+suffix, testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   dispatcherID,
	})

	// Waiting for this agent's acceptance.
	acceptingIssue := dbfx.Issue(t, "session tasks accepting "+suffix)
	versions := []map[string]any{contractVersion(1, agentID, dispatcherID)}
	insertIssueContract(t, acceptingIssue, versions, []map[string]any{pendingDelivery(1, dispatcherID)}, nil)
	// Decided: no pending delivery left.
	decidedIssue := dbfx.Issue(t, "session tasks decided "+suffix)
	decidedVersions := []map[string]any{contractVersion(1, agentID, dispatcherID)}
	decidedDeliveries, decidedDecisions := decidedDelivery(1, dispatcherID)
	insertIssueContract(t, decidedIssue, decidedVersions, decidedDeliveries, decidedDecisions)
	// A later version moved the acceptor away from this agent.
	reassignedIssue := dbfx.Issue(t, "session tasks reassigned "+suffix)
	reassignedVersions := []map[string]any{
		contractVersion(1, agentID, dispatcherID),
		contractVersion(2, dispatcherID, dispatcherID),
	}
	insertIssueContract(t, reassignedIssue, reassignedVersions, []map[string]any{pendingDelivery(2, dispatcherID)}, nil)

	// Accepted delegation returns this session dispatched and still waits on.
	// The return row names the dispatcher, not the worker; an external agent
	// never owns an agent_task_queue row in production.
	workerAgent := dbfx.Agent(t, "Session Tasks Worker "+suffix, testRuntimeID)
	dispatchReturn := func(issueID, dispatcher, externalSession string) string {
		return dbfx.Insert(t, "issue_delegation_return", testutil.Cols{
			"workspace_id":        testWorkspaceID,
			"issue_id":            issueID,
			"dispatcher_agent_id": dispatcher,
			"return_kind":         "external_session",
			"external_session_id": externalSession,
		})
	}
	returnIssue := dbfx.Issue(t, "session tasks return "+suffix)
	acceptedReturn := dispatchReturn(returnIssue, agentID, sessionID)
	returnTask := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": returnIssue, "status": "running", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id) VALUES ($1, $2)", returnTask, acceptedReturn)
	// An address with no worker run is not an outstanding result.
	acceptedIssue := dbfx.Issue(t, "session tasks accepted "+suffix)
	dispatchReturn(acceptedIssue, agentID, sessionID)
	// Returned: the latest run materialized its result.
	returnedIssue := dbfx.Issue(t, "session tasks returned "+suffix)
	returnedReturn := dispatchReturn(returnedIssue, agentID, sessionID)
	returnedTask := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": returnedIssue, "status": "completed", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, materialized_at) VALUES ($1, $2, now())", returnedTask, returnedReturn)
	insertIssueContract(t, returnedIssue, []map[string]any{contractVersion(1, agentID, workerAgent)}, []map[string]any{pendingDelivery(1, workerAgent)}, nil)
	// A follow-up dispatch on the same accepted address makes it pending
	// again: the latest run has not returned its result.
	reopenedIssue := dbfx.Issue(t, "session tasks reopened "+suffix)
	reopenedReturn := dispatchReturn(reopenedIssue, agentID, sessionID)
	oldTask := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": reopenedIssue, "status": "completed", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, materialized_at) VALUES ($1, $2, now())", oldTask, reopenedReturn)
	followUpTask := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": reopenedIssue, "status": "queued", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id) VALUES ($1, $2)", followUpTask, reopenedReturn)
	// Another session of the same agent has its own accepted address.
	otherSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "return-host-" + suffix,
		"session_id":    "return-session-" + suffix,
		"display_name":  "Return peer",
		"daemon_id":     "daemon-return-peer",
	})
	otherSessionIssue := dbfx.Issue(t, "session tasks other session "+suffix)
	dispatchReturn(otherSessionIssue, agentID, otherSession)
	// Another dispatcher has its own list.
	otherDispatcherIssue := dbfx.Issue(t, "session tasks other dispatcher "+suffix)
	dispatchReturn(otherDispatcherIssue, dispatcherID, dispatcherSession)
	// A cancelled issue can never materialize its result.
	cancelledIssue := dbfx.Issue(t, "session tasks cancelled "+suffix, testutil.Cols{"status": "cancelled"})
	dispatchReturn(cancelledIssue, agentID, sessionID)
	// A result takeover re-points the same outcome task onto this session's
	// accepted return with a takeover link. Once materialized the address is
	// returned; treating the takeover link as "no run" would show it pending
	// forever.
	takeoverIssue := dbfx.Issue(t, "session tasks takeover "+suffix)
	takeoverReturn := dispatchReturn(takeoverIssue, agentID, sessionID)
	takeoverTask := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": takeoverIssue, "status": "completed", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, takeover_id, materialized_at) VALUES ($1, $2, gen_random_uuid(), now())", takeoverTask, takeoverReturn)
	// A takeover that has not materialized yet is still pending.
	pendingTakeoverIssue := dbfx.Issue(t, "session tasks pending takeover "+suffix)
	pendingTakeoverReturn := dispatchReturn(pendingTakeoverIssue, agentID, sessionID)
	pendingTakeoverTask := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": pendingTakeoverIssue, "status": "running", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, takeover_id) VALUES ($1, $2, gen_random_uuid())", pendingTakeoverTask, pendingTakeoverReturn)
	waitingIssues := []string{}
	for _, status := range []string{"waiting_local_directory", "deferred"} {
		issue := dbfx.Issue(t, "session tasks "+status+suffix)
		address := dispatchReturn(issue, agentID, sessionID)
		task := dbfx.Task(t, workerAgent, testutil.Cols{"issue_id": issue, "status": status, "runtime_id": testRuntimeID})
		dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id) VALUES ($1, $2)", task, address)
		waitingIssues = append(waitingIssues, issue)
	}

	code, resp := getSessionTasks(t, sessionTasksRequest(ownerID, agentID, true, sessionID))
	if code != http.StatusOK {
		t.Fatalf("session read status = %d, want 200", code)
	}
	if resp.AgentID != agentID || resp.WorkspaceID != testWorkspaceID {
		t.Fatalf("response identity = %q/%q, want agent %s workspace %s", resp.AgentID, resp.WorkspaceID, agentID, testWorkspaceID)
	}
	if resp.SessionID != sessionID {
		t.Fatalf("session read session id = %q, want %s", resp.SessionID, sessionID)
	}

	ownedRoles := sessionTaskRoles(resp.Owned)
	if ownedRoles[executorIssue] != "executor" {
		t.Fatalf("executor role = %q, want executor (owned %+v)", ownedRoles[executorIssue], resp.Owned)
	}
	if ownedRoles[rootIssue] != "leader" || ownedRoles[childIssue] != "leader" {
		t.Fatalf("leader roles = %q/%q, want leader (owned %+v)", ownedRoles[rootIssue], ownedRoles[childIssue], resp.Owned)
	}
	if _, ok := ownedRoles[foreignIssue]; ok {
		t.Fatalf("foreign agent issue appeared in owned list: %+v", resp.Owned)
	}
	var executorItem SessionTaskItem
	for _, item := range resp.Owned {
		if item.IssueID == executorIssue {
			executorItem = item
		}
	}
	if executorItem.Identifier == "" || executorItem.URL != "https://app.example.test/handler-tests/issues/"+executorItem.Identifier {
		t.Fatalf("executor item link = %q/%q, want the workspace issue URL", executorItem.Identifier, executorItem.URL)
	}
	if executorItem.Status != "in_progress" || executorItem.StatusCategory != "started" {
		t.Fatalf("executor status = %q/%q, want in_progress in the started category", executorItem.Status, executorItem.StatusCategory)
	}
	for _, item := range resp.Owned {
		if item.IssueID == rootIssue && (item.ChildCount != 1 || item.ClosedChildCount != 0) {
			t.Fatalf("root child counts = %d/%d, want 0/1", item.ClosedChildCount, item.ChildCount)
		}
	}

	acceptingRoles := sessionTaskRoles(resp.Accepting)
	if acceptingRoles[acceptingIssue] != "acceptor" {
		t.Fatalf("accepting roles = %+v, want the pending contract", acceptingRoles)
	}
	if _, ok := acceptingRoles[decidedIssue]; ok {
		t.Fatalf("decided contract stayed on the acceptance list: %+v", resp.Accepting)
	}
	if _, ok := acceptingRoles[reassignedIssue]; ok {
		t.Fatalf("contract whose current acceptor moved appeared: %+v", resp.Accepting)
	}
	for _, item := range resp.Accepting {
		if item.IssueID == acceptingIssue && item.DeliveryAt == "" {
			t.Fatal("pending delivery has no delivery time")
		}
	}

	returningByIssue := map[string]SessionTaskItem{}
	for _, item := range resp.Returning {
		returningByIssue[item.IssueID] = item
	}
	if len(resp.Returning) != 5 {
		t.Fatalf("returning = %+v, want the five pending issues", resp.Returning)
	}
	for _, want := range append([]string{returnIssue, reopenedIssue, pendingTakeoverIssue}, waitingIssues...) {
		if _, ok := returningByIssue[want]; !ok {
			t.Fatalf("returning %s missing: %+v", want, resp.Returning)
		}
	}
	for _, unwanted := range []string{acceptedIssue, returnedIssue, otherSessionIssue, otherDispatcherIssue, cancelledIssue, takeoverIssue} {
		if _, ok := returningByIssue[unwanted]; ok {
			t.Fatalf("returning includes %s: %+v", unwanted, resp.Returning)
		}
	}
	pending := returningByIssue[returnIssue]
	if pending.Role != "return" || pending.TaskStatus != "running" || pending.AcceptedAt == "" {
		t.Fatalf("returning item = %+v, want role, task status and accepted time", pending)
	}
	if pending.ExecutorName != "Session Tasks Worker "+suffix || pending.ActivityAt == "" {
		t.Fatalf("returning executor/time = %q/%q", pending.ExecutorName, pending.ActivityAt)
	}
	var returnedDelegation *SessionTaskItem
	for i := range resp.Delegated {
		if resp.Delegated[i].IssueID == returnedIssue {
			returnedDelegation = &resp.Delegated[i]
		}
	}
	if returnedDelegation == nil || !returnedDelegation.PendingAcceptance {
		t.Fatalf("returned issue awaiting acceptance missing from delegated list: %+v", resp.Delegated)
	}
	if reopened := returningByIssue[reopenedIssue]; reopened.TaskStatus != "queued" {
		t.Fatalf("reopened returning item = %+v, want the follow-up run status", reopened)
	}

	// Even the workspace owner cannot read an agent's session summary through
	// this endpoint: it is the session's own view, not a management surface.
	if code, _ := getSessionTasks(t, sessionTasksRequest(testUserID, agentID, false, "")); code != http.StatusForbidden {
		t.Fatalf("human read status = %d, want 403", code)
	}
	// A session credential without its session binding is refused rather than
	// silently widened to every session of the agent.
	if code, _ := getSessionTasks(t, sessionTasksRequest(ownerID, agentID, true, "")); code != http.StatusForbidden {
		t.Fatalf("unbound session credential status = %d, want 403", code)
	}
	otherAgent, otherSession, _ := externalAgentFixture(t, "Session Tasks Other", "daemon-session-tasks-other")
	crossReq := newRequestAs(ownerID, http.MethodGet, "/api/agents/"+agentID+"/session-tasks", nil)
	crossReq.Header.Set("X-Actor-Source", "external_agent")
	crossReq.Header.Set("X-Agent-ID", otherAgent)
	crossReq.Header.Set(ActorSessionHeader, otherSession)
	if code, _ := getSessionTasks(t, withURLParam(crossReq, "id", agentID)); code != http.StatusForbidden {
		t.Fatalf("sibling credential read status = %d, want 403", code)
	}
	plain := dbfx.User(t, "Session Tasks Plain", "session-tasks-plain-"+suffix+"@test.local")
	dbfx.Member(t, testWorkspaceID, plain, "member")
	if code, _ := getSessionTasks(t, sessionTasksRequest(plain, agentID, false, "")); code != http.StatusForbidden {
		t.Fatalf("plain member read status = %d, want 403", code)
	}

	// A foreign workspace agent is not visible through this workspace.
	otherWorkspace := dbfx.Workspace(t, "Session Tasks Other Workspace", "session-tasks-other-ws-"+suffix)
	foreignWorkspaceAgent := dbfx.Agent(t, "Session Tasks Foreign Workspace", "", testutil.Cols{
		"workspace_id":    otherWorkspace,
		"owner_id":        ownerID,
		"execution_kind":  "external",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	foreignReq := sessionTasksRequest(ownerID, foreignWorkspaceAgent, true, sessionID)
	if code, _ := getSessionTasks(t, foreignReq); code != http.StatusNotFound {
		t.Fatalf("foreign workspace agent status = %d, want 404", code)
	}
}

func TestSessionReturnsFollowDelegationLifecycle(t *testing.T) {
	ctx := context.Background()
	dispatcher, session, owner := externalAgentFixture(t, "Session lifecycle", "session-lifecycle-daemon")
	var hostSession string
	dbfx.QueryRow(t, `SELECT session_id FROM external_agent_session WHERE id=$1`, session).Scan(&hostSession)
	token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "session-lifecycle")
	create := func(assignee string) string {
		t.Helper()
		title := fmt.Sprintf("session lifecycle %d", time.Now().UnixNano())
		bumpIssueCounter(t)
		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, assignee), nil, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, hostSession) })
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		id, _ := issueIDByTitle(t, title)
		cleanupDelegatedIssue(t, id)
		return id
	}
	assertPending := func(issue string, want bool) {
		t.Helper()
		code, response := getSessionTasks(t, sessionTasksRequest(owner, dispatcher, true, session))
		if code != http.StatusOK {
			t.Fatalf("session tasks: %d", code)
		}
		_, got := sessionTaskRoles(response.Returning)[issue]
		if got != want {
			t.Fatalf("issue %s pending=%v want=%v: %+v", issue, got, want, response.Returning)
		}
	}
	self := create(dispatcher)
	assertPending(self, false)
	issue := create(target)
	assertPending(issue, true)
	resultID := latestTaskIDFor(t, issue, target)
	result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(resultID))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	assertPending(issue, false)
	rerun := func() {
		t.Helper()
		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost, "/api/issues/"+issue+"/rerun", map[string]any{}, map[string]string{"id": issue}, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, hostSession) })
		if w.Code != http.StatusAccepted {
			t.Fatalf("rerun: %d %s", w.Code, w.Body.String())
		}
	}
	rerun()
	assertPending(issue, true)
	newID := latestTaskIDFor(t, issue, target)
	if _, err := testHandler.TaskService.CancelTask(service.WithTerminalReport(ctx), parseUUID(newID)); err != nil {
		t.Fatal(err)
	}
	assertPending(issue, false)
	rerun()
	assertPending(issue, true)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch, "/api/issues/"+issue, map[string]any{"status": "done"}, map[string]string{"id": issue}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}
	assertPending(issue, false)
}

func TestSessionTasksAttentionEvidence(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agent, session, owner := externalAgentFixture(t, "Attention recipient", "attention-recipient")
	worker := dbfx.Agent(t, "Attention executor", testRuntimeID)
	other := dbfx.Agent(t, "Attention other", testRuntimeID)
	issue := dbfx.Issue(t, "Open delegated issue", testutil.Cols{"assignee_type": "agent", "assignee_id": worker, "status": "blocked"})
	address := dbfx.Insert(t, "issue_delegation_return", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "dispatcher_agent_id": agent, "return_kind": "external_session", "external_session_id": session})
	task := dbfx.Task(t, worker, testutil.Cols{"issue_id": issue, "status": "running", "runtime_id": testRuntimeID})
	dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id) VALUES ($1, $2)", task, address)
	at := time.Now().UTC().Add(-time.Hour)
	comment := func(issueID, author, content string, when time.Time, parent string) string {
		cols := testutil.Cols{"author_type": "agent", "author_id": author, "created_at": when}
		if parent != "" {
			cols["parent_id"] = parent
		}
		return dbfx.Comment(t, issueID, content, cols)
	}
	mention := fmt.Sprintf("[@Recipient](mention://agent/%s)", agent)
	pending := comment(issue, worker, fmt.Sprintf("[@Recipient](mention://agent/%s) please decide", strings.ToUpper(agent)), at, "")
	replied := comment(issue, worker, mention+" answered", at.Add(time.Minute), "")
	nested := comment(issue, other, "context", at.Add(2*time.Minute), replied)
	answer := comment(issue, agent, "answer", at.Add(3*time.Minute), nested)
	dbfx.Exec(t, "UPDATE comment SET type = 'progress_update' WHERE id = $1", answer)
	comment(issue, worker, "plain @Recipient", at.Add(4*time.Minute), "")
	comment(issue, worker, "[@Someone](other)\n[Recipient](mention://agent/"+agent+")", at.Add(4*time.Minute), "")
	comment(issue, worker, "[@Someone](other) [Recipient](mention://agent/"+agent+")", at.Add(4*time.Minute), "")
	comment(issue, other, mention+" other author", at.Add(5*time.Minute), "")
	comment(issue, agent, "reply in another thread", at.Add(5*time.Minute), "")
	resolved := comment(issue, worker, mention+" resolved", at.Add(5*time.Minute), "")
	dbfx.Exec(t, "UPDATE comment SET resolved_at = now(), resolved_by_type = 'agent', resolved_by_id = $1 WHERE id = $2", agent, resolved)
	comment(issue, worker, "Waiting for input\nmore detail", at.Add(6*time.Minute), "")
	closed := dbfx.Issue(t, "Closed issue", testutil.Cols{"assignee_type": "agent", "assignee_id": worker, "status": "done"})
	comment(closed, worker, mention+" closed", at, "")
	foreignWorkspace := dbfx.Workspace(t, "Attention foreign workspace", fmt.Sprintf("attention-%d", time.Now().UnixNano()))
	dbfx.Cleanup(t, "DELETE FROM workspace WHERE id = $1", foreignWorkspace)
	foreignIssue := dbfx.Issue(t, "Foreign request", testutil.Cols{"workspace_id": foreignWorkspace, "assignee_type": "agent", "assignee_id": worker})
	dbfx.Comment(t, foreignIssue, mention+" foreign", testutil.Cols{"workspace_id": foreignWorkspace, "author_type": "agent", "author_id": worker})
	owned := dbfx.Issue(t, "Production-shaped parent", testutil.Cols{"assignee_type": "agent", "assignee_id": agent, "status": "in_progress"})
	for i := 0; i < 8; i++ {
		status := "done"
		if i < 2 {
			status = "cancelled"
		}
		dbfx.Issue(t, fmt.Sprintf("Closed child %d", i), testutil.Cols{"parent_issue_id": owned, "assignee_type": "agent", "assignee_id": agent, "status": status})
	}
	code, response := getSessionTasks(t, sessionTasksRequest(owner, agent, true, session))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(response.Requests) != 1 || response.Requests[0].RequestID != pending || response.Requests[0].IssueID != issue || response.Requests[0].Role != "request" {
		t.Fatalf("requests = %+v, want only %s", response.Requests, pending)
	}
	if len(response.Delegated) != 1 || response.Delegated[0].BlockedReason != "Waiting for input" {
		t.Fatalf("delegated = %+v, want executor comment first line", response.Delegated)
	}
	if len(response.Owned) != 1 || response.Owned[0].IssueID != owned || response.Owned[0].ChildCount != 8 || response.Owned[0].ClosedChildCount != 8 {
		t.Fatalf("owned = %+v, want only open parent with 8/8 closed children", response.Owned)
	}
}

func TestSessionPullRequestsScopeAndDescendants(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "PR session", "pr-session-daemon")
	other, otherSession, otherOwner := externalAgentFixture(t, "Other PR session", "other-pr-session-daemon")
	const installationID int64 = 30264988
	secret := "session-pr-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	root := prAutoCompleteTestIssue(t, "session root", installationID)
	assign := map[string]any{"assignee_type": "agent", "assignee_id": agent, "recipient_session_id": session, "suppress_run": true}
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+root.ID, assign), "id", root.ID)).Want(http.StatusOK)
	var child IssueResponse
	testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "session child", "parent_issue_id": root.ID})).Want(http.StatusCreated).JSON(&child)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE issue_id = $1`, child.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, child.ID)
	})
	firePRWebhook(t, secret, installationID, 8, child.Identifier+" fix", "", "fix", "opened")
	read := func(user, actor, sid string, external bool) *httptest.ResponseRecorder {
		req := sessionTasksRequest(user, actor, external, sid)
		w := httptest.NewRecorder()
		testHandler.GetAgentSessionPullRequests(w, req)
		return w
	}
	w := read(owner, agent, session, true)
	if w.Code != http.StatusOK {
		t.Fatalf("session read: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		PullRequests []descendantPullRequest `json:"pull_requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.PullRequests) != 1 || body.PullRequests[0].SourceIssueID != child.ID {
		t.Fatalf("projection: %+v", body)
	}
	w = read(otherOwner, other, otherSession, true)
	if w.Code != http.StatusOK {
		t.Fatalf("other session: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.PullRequests) != 0 {
		t.Fatalf("other session saw PRs: %+v", body)
	}
	if w = read(owner, agent, "", false); w.Code != http.StatusForbidden {
		t.Fatalf("human read: %d", w.Code)
	}
}
