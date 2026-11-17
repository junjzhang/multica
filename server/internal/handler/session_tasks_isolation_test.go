package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestSessionTasksSharedAgentIsolation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agent, first, owner := externalAgentFixture(t, "Shared session tasks", "shared-tasks")
	sessions := []string{first}
	for n := 1; n < 3; n++ {
		sessions = append(sessions, dbfx.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": testWorkspaceID, "owner_user_id": owner, "agent_id": agent, "provider": "paseo", "host_id": "shared-host", "session_id": fmt.Sprintf("shared-%d-%d", time.Now().UnixNano(), n), "display_name": "Shared", "daemon_id": "shared-tasks"}))
	}
	worker := dbfx.Agent(t, "Shared worker", testRuntimeID)
	owned, accepting, requested, delegated := []string{}, []string{}, []string{}, []string{}
	for _, session := range sessions[:2] {
		issue := dbfx.Issue(t, "Bound executor", testutil.Cols{"assignee_type": "agent", "assignee_id": agent, "status": "in_progress"})
		dbfx.Insert(t, "external_agent_session_binding", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "agent_id": agent, "session_id": session})
		owned = append(owned, issue)
		issue = dbfx.Issue(t, "Bound acceptance", testutil.Cols{"assignee_type": "agent", "assignee_id": worker})
		v := contractVersion(1, agent, worker)
		v["acceptor"].(map[string]any)["session_id"] = session
		insertIssueContract(t, issue, []map[string]any{v}, []map[string]any{pendingDelivery(1, worker)}, nil)
		accepting = append(accepting, issue)
		comment := dbfx.Comment(t, issue, fmt.Sprintf("[@Recipient](mention://agent/%s) decide", agent), testutil.Cols{"author_type": "agent", "author_id": worker})
		dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "comment_id": comment, "recipient_agent_id": agent, "target_session_id": session, "sender_type": "agent", "sender_id": worker})
		requested = append(requested, comment)
		address := dbfx.Insert(t, "issue_delegation_return", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "dispatcher_agent_id": agent, "return_kind": "external_session", "external_session_id": session})
		task := dbfx.Task(t, worker, testutil.Cols{"issue_id": issue, "status": "running", "runtime_id": testRuntimeID})
		dbfx.Exec(t, "INSERT INTO agent_task_delegation_return (task_id, delegation_return_id) VALUES ($1, $2)", task, address)
		delegated = append(delegated, issue)
	}
	dbfx.Issue(t, "Unbound historical executor", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})
	unbound := dbfx.Issue(t, "Unbound historical acceptance")
	insertIssueContract(t, unbound, []map[string]any{contractVersion(1, agent, worker)}, []map[string]any{pendingDelivery(1, worker)}, nil)
	for n, session := range sessions {
		code, got := getSessionTasks(t, sessionTasksRequest(owner, agent, true, session))
		if code != http.StatusOK {
			t.Fatalf("session %d status %d", n, code)
		}
		if n == 2 {
			if len(got.Owned)+len(got.Accepting)+len(got.Requests)+len(got.Returning)+len(got.Delegated) != 0 {
				t.Errorf("unassigned session leaked tasks: %+v", got)
			}
			continue
		}
		if len(got.Owned) != 1 || got.Owned[0].IssueID != owned[n] || got.Owned[0].Role != "executor" {
			t.Errorf("session %d owned: %+v", n, got.Owned)
		}
		if len(got.Accepting) != 1 || got.Accepting[0].IssueID != accepting[n] {
			t.Errorf("session %d accepting: %+v", n, got.Accepting)
		}
		if len(got.Requests) != 1 || got.Requests[0].RequestID != requested[n] {
			t.Errorf("session %d requests: %+v", n, got.Requests)
		}
		if len(got.Returning) != 1 || got.Returning[0].IssueID != delegated[n] || len(got.Delegated) != 1 || got.Delegated[0].IssueID != delegated[n] {
			t.Errorf("session %d returns/delegated: %+v / %+v", n, got.Returning, got.Delegated)
		}
	}
}

func TestSessionTasksExecutorAndLeaderSessions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agent, leaderSession, owner := externalAgentFixture(t, "Shared leadership", "shared-leadership")
	executorSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": testWorkspaceID, "owner_user_id": owner, "agent_id": agent, "provider": "paseo", "host_id": "leadership-host", "session_id": fmt.Sprint(time.Now().UnixNano()), "display_name": "Executor", "daemon_id": "shared-leadership"})
	root := dbfx.Issue(t, "Leader root", testutil.Cols{"assignee_type": "agent", "assignee_id": agent, "leader_agent_id": agent, "leader_external_session_id": leaderSession})
	dbfx.Exec(t, "UPDATE issue SET leadership_root_issue_id = id WHERE id = $1", root)
	binding := dbfx.Insert(t, "external_agent_session_binding", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": root, "agent_id": agent, "session_id": executorSession})
	child := dbfx.Issue(t, "Leader child", testutil.Cols{"parent_issue_id": root, "leadership_root_issue_id": root})
	code, leader := getSessionTasks(t, sessionTasksRequest(owner, agent, true, leaderSession))
	if code != http.StatusOK || len(leader.Owned) != 2 || sessionTaskRoles(leader.Owned)[root] != "leader" || sessionTaskRoles(leader.Owned)[child] != "leader" {
		t.Fatalf("leader projection: %d %+v", code, leader.Owned)
	}
	code, executor := getSessionTasks(t, sessionTasksRequest(owner, agent, true, executorSession))
	if code != http.StatusOK || len(executor.Owned) != 1 || executor.Owned[0].IssueID != root || executor.Owned[0].Role != "executor" {
		t.Fatalf("executor projection: %d %+v", code, executor.Owned)
	}
	dbfx.Exec(t, "UPDATE external_agent_session_binding SET revoked_at = now() WHERE id = $1", binding)
	_, executor = getSessionTasks(t, sessionTasksRequest(owner, agent, true, executorSession))
	if len(executor.Owned) != 0 {
		t.Fatalf("revoked executor leaked tasks: %+v", executor.Owned)
	}
}
