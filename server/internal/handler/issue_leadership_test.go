package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/issueleader"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type leadershipFixture struct{ agent, a, b, owner, squad, defaultLeader, tokenA, tokenB string }

func taskLeadershipFixture(t *testing.T) leadershipFixture {
	t.Helper()
	agent, a, owner := externalAgentFixture(t, "Task leadership template", "leadership-daemon")
	b := dbfx.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "owner_user_id": owner, "provider": "paseo", "host_id": "leadership-host", "session_id": uuidToString(dbid.NewV7()), "daemon_id": "leadership-daemon"})
	defaultLeader := dbfx.Agent(t, "Managed default leader", testRuntimeID)
	squad := dbfx.Squad(t, "Task leadership squad", defaultLeader)
	dbfx.SquadMember(t, squad, "agent", defaultLeader)
	dbfx.SquadMember(t, squad, "agent", agent)
	return leadershipFixture{agent: agent, a: a, b: b, owner: owner, squad: squad, defaultLeader: defaultLeader, tokenA: externalSessionTokenFixture(t, agent, a, owner, time.Now().Add(time.Hour)), tokenB: externalSessionTokenFixture(t, agent, b, owner, time.Now().Add(time.Hour))}
}

func bindTaskLeadership(t *testing.T, issue, squad, agent, session string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET leadership_root_issue_id=id,leadership_squad_id=$2,leader_agent_id=$3,leader_external_session_id=$4,leadership_revision=1 WHERE id=$1`, issue, squad, agent, session)
	bindExternalSession(t, agent, session, issue)
}

func (f leadershipFixture) create(t *testing.T, token string) string {
	t.Helper()
	rec := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", map[string]any{"title": "Session-owned squad task " + uuidToString(dbid.NewV7()), "assignee_type": "squad", "assignee_id": f.squad}, nil, nil)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var issue IssueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	cleanupDelegatedIssue(t, issue.ID)
	return issue.ID
}

func TestTaskLeadershipIndependentSessionsAndDefaultRotation(t *testing.T) {
	f := taskLeadershipFixture(t)
	for _, test := range []struct{ token, session string }{{f.tokenA, f.a}, {f.tokenB, f.b}} {
		issue := f.create(t, test.token)
		var session, recipient, returned string
		dbfx.QueryRow(t, `SELECT leader_external_session_id FROM issue WHERE id=$1`, issue).Scan(&session)
		dbfx.QueryRow(t, `SELECT target_session_id FROM agent_mailbox_message WHERE issue_id=$1 AND kind='issue_assignment'`, issue).Scan(&recipient)
		dbfx.QueryRow(t, `SELECT external_session_id FROM issue_delegation_return WHERE issue_id=$1`, issue).Scan(&returned)
		if session != test.session || recipient != session || returned != session {
			t.Fatalf("leader/dispatch/return=%s/%s/%s", session, recipient, returned)
		}
		dbfx.Exec(t, `UPDATE issue SET coordinator_type='member',coordinator_id=$2 WHERE id=$1`, issue, testUserID)
		contractCall(t, issue, definition(0), "").Want(200)
		submit := contractRequest{Action: "submit", ExpectedSequence: 1, Version: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Independent session evidence"}
		other := f.tokenA
		if test.session == f.a {
			other = f.tokenB
		}
		authenticatedContractCall(t, issue, submit, other).Want(409)
		defaultTask := dbfx.Task(t, f.defaultLeader, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issue, "status": "running"})
		authenticatedContractCall(t, issue, submit, taskTokenFixture(t, f.defaultLeader, defaultTask)).Want(403)

		authenticatedContractCall(t, issue, submit, test.token).Want(200)
		var view ContractView
		testutil.Call(t, testHandler.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/", nil), "id", issue)).Want(200).JSON(&view)
		if view.Contract.Pending().Author.SessionID != session {
			t.Fatal("delivery lost author session")
		}
		newDefault := dbfx.Agent(t, "Rotated managed default "+test.session, testRuntimeID)
		dbfx.SquadMember(t, f.squad, "agent", newDefault)
		dbfx.Exec(t, `UPDATE squad SET leader_id=$1 WHERE id=$2`, newDefault, f.squad)
		stored, _ := testHandler.Queries.GetIssue(t.Context(), parseUUID(issue))
		squad, _ := testHandler.Queries.GetSquad(t.Context(), parseUUID(f.squad))
		leader, err := issueleader.ResolveIssueLeader(t.Context(), testHandler.Queries, stored, squad)
		if err != nil || uuidToString(leader.SessionID) != session {
			t.Fatalf("default rotation drifted task: %+v %v", leader, err)
		}
	}
}

func TestTaskLeadershipSiblingCommentsDoNotDispatch(t *testing.T) {
	f := taskLeadershipFixture(t)
	issue := f.create(t, f.tokenA)
	wake := dbfx.Insert(t, "issue_wakeup", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "agent_id": f.agent, "created_by": f.owner, "id": dbid.NewV7(), "instruction": "Observe ordinary comments", "kind": "event", "mode": "continuous", "event_types": []string{"comment.created"}, "enabled": true})
	before := dbfx.Count(t, "SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1", issue)
	path := "/api/issues/" + issue + "/comments"
	rec := serveAuthenticated(t, f.tokenB, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, path, map[string]any{"content": "A plain sibling comment"}, map[string]string{"id": issue}, nil)
	if rec.Code != 201 {
		t.Fatalf("ordinary comment: %d %s", rec.Code, rec.Body.String())
	}
	var comment CommentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &comment)
	var session string
	dbfx.QueryRow(t, "SELECT actor_session_id FROM comment WHERE id=$1", comment.ID).Scan(&session)
	if session != f.b {
		t.Fatal("comment lost actual session author")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1", wake); n != 0 {
		t.Fatal("sibling ordinary comment created executable wakeup receipts")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1", issue); n != before {
		t.Fatalf("ordinary sibling comment dispatched %d messages", n-before)
	}
	for _, handler := range []http.HandlerFunc{testHandler.CreateComment, testHandler.PreviewCommentTriggers} {
		rec = serveAuthenticated(t, f.tokenB, handler, http.MethodPost, path, map[string]any{"content": fmt.Sprintf("[@squad](mention://squad/%s) do work", f.squad)}, map[string]string{"id": issue}, nil)
		if rec.Code != 409 {
			t.Fatalf("sibling dispatch: %d %s", rec.Code, rec.Body.String())
		}
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1", issue); n != 1 {
		t.Fatal("refused dispatch persisted a comment")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1", issue); n != 1 {
		t.Fatal("sibling changed return relationships")
	}
}

func TestTaskLeadershipClaimCAS(t *testing.T) {
	f := taskLeadershipFixture(t)
	issue := dbfx.Issue(t, "Concurrent leadership claim", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	var revision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", issue).Scan(&revision)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, token := range []string{f.tokenA, f.tokenB} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			rec := serveAuthenticated(t, token, http.HandlerFunc(testHandler.ClaimIssueLeadership), http.MethodPost, "/api/issues/"+issue+"/leadership/claim", map[string]any{"expected_revision": 0, "expected_issue_revision": revision}, map[string]string{"id": issue}, nil)
			codes <- rec.Code
		}(token)
	}
	wg.Wait()
	close(codes)
	wins, conflicts := 0, 0
	for code := range codes {
		switch code {
		case 200:
			wins++
		case 409:
			conflicts++
		default:
			t.Errorf("claim status=%d", code)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM external_agent_session_binding WHERE issue_id=$1 AND revoked_at IS NULL", issue); n != 1 {
		t.Fatalf("CAS left %d active bindings", n)
	}
}

func TestTaskLeadershipHandoffIsExplicitAndStalesContract(t *testing.T) {
	f := taskLeadershipFixture(t)
	issue := f.create(t, f.tokenA)
	dbfx.Exec(t, `UPDATE issue SET coordinator_type='member',coordinator_id=$2 WHERE id=$1`, issue, testUserID)
	contractCall(t, issue, definition(0), "").Want(200)
	var revision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", issue).Scan(&revision)
	req := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedRevision: 1, ExpectedIssueRevision: revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: f.agent, SessionID: f.b, Reason: "Human explicitly changes the responsible session"}
	path := "/api/issues/" + issue + "/leadership/handoff"
	rec := serveAuthenticated(t, f.tokenA, http.HandlerFunc(testHandler.HandoffIssueLeadership), http.MethodPost, path, req, map[string]string{"id": issue}, nil)
	if rec.Code != 403 {
		t.Fatalf("machine handoff: %d", rec.Code)
	}
	for range 2 {
		testutil.Call(t, testHandler.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, path, req), "id", issue)).Want(200)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='leadership_handoff'", issue); n != 1 {
		t.Fatalf("handoff retries wrote %d events", n)
	}
	var view ContractView
	testutil.Call(t, testHandler.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/", nil), "id", issue)).Want(200).JSON(&view)
	if !view.Stale {
		t.Fatal("handoff did not stale contract")
	}
	submit := contractRequest{Action: "submit", ExpectedSequence: 1, Version: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Old session must fail"}
	authenticatedContractCall(t, issue, submit, f.tokenA).Want(409)
	authenticatedContractCall(t, issue, submit, f.tokenB).Want(409)
	var frozen string
	dbfx.QueryRow(t, "SELECT external_session_id FROM issue_delegation_return WHERE issue_id=$1", issue).Scan(&frozen)
	if frozen != f.a {
		t.Fatal("handoff rewrote accepted return")
	}
}

func TestTaskLeadershipRejectsExternalDefaultConfiguration(t *testing.T) {
	f := taskLeadershipFixture(t)
	testutil.Call(t, testHandler.CreateSquad, squadScopeReq(testUserID, http.MethodPost, "/api/squads", map[string]any{"name": "Invalid external default", "leader_id": f.agent}, nil)).Want(400)
	testutil.Call(t, testHandler.UpdateSquad, squadScopeReq(testUserID, http.MethodPatch, "/api/squads", map[string]any{"leader_id": f.agent}, map[string]string{"id": f.squad})).Want(400)
	dbfx.Exec(t, "UPDATE squad SET leader_id=$1 WHERE id=$2", f.agent, f.squad)
	var legacy SquadResponse
	testutil.Call(t, testHandler.GetSquad, squadScopeReq(testUserID, http.MethodGet, "/api/squads", nil, map[string]string{"id": f.squad})).Want(200).JSON(&legacy)
	if legacy.DefaultLeaderUnavailableReason != "default_leader_must_be_managed" {
		t.Fatal("legacy external default lacks a read-only migration diagnostic")
	}

	testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "Legacy default must not dispatch", "assignee_type": "squad", "assignee_id": f.squad})).Want(400)
	// External initiation uses the actual qualified session even before a
	// human replaces a legacy invalid default.
	f.create(t, f.tokenA)
}

func TestTaskLeadershipRequesterSessionCannotReviseOrAcceptForSibling(t *testing.T) {
	f := taskLeadershipFixture(t)
	executor := dbfx.Agent(t, "Session contract executor", testRuntimeID)
	issue := dbfx.Issue(t, "Session-pinned requester", testutil.Cols{"creator_type": "agent", "creator_id": f.agent, "creator_session_id": f.a, "coordinator_type": "agent", "coordinator_id": f.agent, "assignee_type": "agent", "assignee_id": executor})
	bindExternalSession(t, f.agent, f.a, issue)
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	authenticatedContractCall(t, issue, definition(0), f.tokenB).Want(409)
	authenticatedContractCall(t, issue, definition(0), f.tokenA).Want(200)
	submit := contractRequest{Action: "submit", ExpectedSequence: 1, Version: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Managed evidence"}
	contractCall(t, issue, submit, executor).Want(200)
	accept := contractRequest{Action: "accept", ExpectedSequence: 2, Version: 1, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0}}
	authenticatedContractCall(t, issue, accept, f.tokenB).Want(409)
	authenticatedContractCall(t, issue, accept, f.tokenA).Want(200)
}

func TestTaskLeadershipCrossSquadReassignmentChecksReferencingChain(t *testing.T) {
	for _, referenced := range []bool{false, true} {
		t.Run(fmt.Sprint(referenced), func(t *testing.T) {
			f := taskLeadershipFixture(t)
			issue := f.create(t, f.tokenA)
			nextLeader := dbfx.Agent(t, "Target squad default", testRuntimeID)
			nextSquad := dbfx.Squad(t, "Target squad", nextLeader)
			child := ""
			if referenced {
				child = dbfx.Issue(t, "Explicit leadership child", testutil.Cols{"parent_issue_id": issue, "leadership_root_issue_id": issue})
			}
			body := map[string]any{"assignee_type": "squad", "assignee_id": nextSquad}
			want := 200
			if referenced {
				want = 409
			}
			rec := testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, body), "id", issue)).Want(want)
			var squad, leader string
			var revision int64
			dbfx.QueryRow(t, "SELECT leadership_squad_id,leader_agent_id,leadership_revision FROM issue WHERE id=$1", issue).Scan(&squad, &leader, &revision)
			if referenced {
				if !strings.Contains(rec.Body.String(), child) || squad != f.squad || revision != 1 {
					t.Fatal("rejected root reassignment lost child-chain diagnostics or changed leadership")
				}
			} else if squad != nextSquad || leader != nextLeader || revision != 2 {
				t.Fatalf("target root=%s/%s/%d", squad, leader, revision)
			}
		})
	}
}

func TestTaskExternalExecutorHandoffPreservesHistoryAndStalesContract(t *testing.T) {
	f := taskLeadershipFixture(t)
	issue := dbfx.Issue(t, "Direct external executor handoff", testutil.Cols{"assignee_type": "agent", "assignee_id": f.agent, "coordinator_type": "member", "coordinator_id": testUserID})
	bindExternalSession(t, f.agent, f.a, issue)
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	contractCall(t, issue, definition(0), "").Want(200)
	var revision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", issue).Scan(&revision)
	body := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedIssueRevision: revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: f.agent, SessionID: f.b, Reason: "Recover direct executor through explicit human handoff"}
	for range 2 {
		testutil.Call(t, testHandler.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/leadership/handoff", body), "id", issue)).Want(200)
	}
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Direct executor evidence"}
	authenticatedContractCall(t, issue, submit, f.tokenA).Want(409)
	authenticatedContractCall(t, issue, submit, f.tokenB).Want(409)
	contractCall(t, issue, definition(1), "").Want(200)
	submit.Version = 2
	submit.ExpectedSequence = 2
	authenticatedContractCall(t, issue, submit, f.tokenB).Want(200)
	if n := dbfx.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='executor_handoff'", issue); n != 1 {
		t.Fatalf("direct handoff audit rows=%d", n)
	}
}
