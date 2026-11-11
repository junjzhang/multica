package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
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
	var originalReturn string
	dbfx.QueryRow(t, "SELECT id FROM issue_delegation_return WHERE issue_id=$1 AND is_current", issue).Scan(&originalReturn)
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
	dbfx.QueryRow(t, "SELECT external_session_id FROM issue_delegation_return WHERE id=$1", originalReturn).Scan(&frozen)
	if frozen != f.a {
		t.Fatal("handoff rewrote accepted return")
	}
	var currentSession string
	dbfx.QueryRow(t, "SELECT external_session_id FROM issue_delegation_return WHERE issue_id=$1 AND is_current", issue).Scan(&currentSession)
	if currentSession != f.b {
		t.Fatal("handoff did not advance the current dispatcher session")
	}
	worker, _ := executableTarget(t, "New handoff worker")
	rec = serveAuthenticated(t, f.tokenB, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issue+"/comments", map[string]any{"content": fmt.Sprintf("[@Worker](mention://agent/%s) continue after handoff", worker)}, map[string]string{"id": issue}, nil)
	if rec.Code != 201 {
		t.Fatalf("new leader could not delegate after handoff: %d %s", rec.Code, rec.Body.String())
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
	h := *testHandler
	h.Bus = events.New()
	updates := make(chan events.Event, 4)
	h.Bus.Subscribe(protocol.EventIssueUpdated, func(event events.Event) { updates <- event })
	body := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedIssueRevision: revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: f.agent, SessionID: f.b, Reason: "Recover direct executor through explicit human handoff"}
	for range 2 {
		testutil.Call(t, h.HandoffIssueExecutor, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/executor/handoff", body), "id", issue)).Want(200)
	}
	if len(updates) != 1 {
		t.Fatalf("idempotent handoff published %d issue updates", len(updates))
	}
	event := <-updates
	response := event.Payload.(map[string]any)["issue"].(IssueResponse)
	if event.WorkspaceID != testWorkspaceID || event.ActorType != "member" || response.ID != issue || response.Revision != revision+1 {
		t.Fatalf("handoff published the wrong issue update: %+v", event)
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

// A handoff that moves the executor to a different agent is an assignment
// change: subscriber, activity and notification listeners only run for
// assignee_changed, so the event must carry it and the previous assignee.
func TestExternalExecutorHandoffPublishesAssigneeTransition(t *testing.T) {
	f := taskLeadershipFixture(t)
	issue := dbfx.Issue(t, "Executor handoff to a different agent", testutil.Cols{"assignee_type": "agent", "assignee_id": f.agent})
	bindExternalSession(t, f.agent, f.a, issue)
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	var revision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", issue).Scan(&revision)
	target := dbfx.Agent(t, "Managed handoff target", testRuntimeID)
	h := *testHandler
	h.Bus = events.New()
	updates := make(chan events.Event, 4)
	h.Bus.Subscribe(protocol.EventIssueUpdated, func(event events.Event) { updates <- event })
	body := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedIssueRevision: revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: target, Reason: "Move the responsible executor to a managed agent"}
	testutil.Call(t, h.HandoffIssueExecutor, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/executor/handoff", body), "id", issue)).Want(200)
	if len(updates) != 1 {
		t.Fatalf("executor handoff published %d issue updates", len(updates))
	}
	payload := (<-updates).Payload.(map[string]any)
	if changed, _ := payload["assignee_changed"].(bool); !changed {
		t.Fatal("executor handoff did not publish assignee_changed; new executor misses subscription and notification")
	}
	prevID, _ := payload["prev_assignee_id"].(*string)
	prevType, _ := payload["prev_assignee_type"].(*string)
	if prevID == nil || *prevID != f.agent || prevType == nil || *prevType != "agent" {
		t.Fatalf("handoff lost the previous assignee: type=%v id=%v", prevType, prevID)
	}
	response := payload["issue"].(IssueResponse)
	if response.AssigneeID == nil || *response.AssigneeID != target {
		t.Fatalf("handoff event carries assignee %v, want %s", response.AssigneeID, target)
	}
}

// A subtask carries only the leadership root pointer; the leader columns live
// on the root row. The detail response must project them from the root instead
// of reporting the task leader unavailable for every child issue.
func TestTaskLeadershipChildIssueProjectsRootLeader(t *testing.T) {
	f := taskLeadershipFixture(t)
	root := f.create(t, f.tokenA)
	child := dbfx.Issue(t, "Child of a task chain", testutil.Cols{
		"parent_issue_id":          root,
		"leadership_root_issue_id": root,
		"leadership_squad_id":      f.squad,
		"assignee_type":            "squad",
		"assignee_id":              f.squad,
	})

	var resp IssueResponse
	testutil.Call(t, testHandler.GetIssue, withURLParam(
		newRequest(http.MethodGet, "/api/issues/"+child, nil), "id", child)).Want(200).JSON(&resp)
	if resp.LeaderAgentID != f.agent || resp.LeaderExternalSessionID != f.a {
		t.Fatalf("child leader = %q/%q, want %q/%q", resp.LeaderAgentID, resp.LeaderExternalSessionID, f.agent, f.a)
	}
	if resp.LeadershipRootIssueID != root {
		t.Fatalf("child leadership root = %q, want %q", resp.LeadershipRootIssueID, root)
	}
	if resp.LeadershipSquadID != f.squad {
		t.Fatalf("child leadership squad = %q, want %q", resp.LeadershipSquadID, f.squad)
	}
	var rootRevision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", root).Scan(&rootRevision)
	if resp.LeadershipRootRevision != rootRevision {
		t.Fatalf("child root revision = %d, want %d", resp.LeadershipRootRevision, rootRevision)
	}
}

// A subtask row carries only the leadership root pointer, so every issue:updated
// payload must project the leader from the root before it leaves the server.
// Without the projection the client's shallow cache merge drops the leader and
// replaces the root revision with the subtask's own revision.
func TestTaskLeadershipChildBroadcastProjectsRootLeader(t *testing.T) {
	f := taskLeadershipFixture(t)
	root := f.create(t, f.tokenA)
	child := dbfx.Issue(t, "Child of a task chain", testutil.Cols{
		"parent_issue_id":          root,
		"leadership_root_issue_id": root,
		"leadership_squad_id":      f.squad,
		"assignee_type":            "squad",
		"assignee_id":              f.squad,
	})
	row, err := testHandler.Queries.GetIssueInWorkspace(t.Context(), db.GetIssueInWorkspaceParams{
		ID:          parseUUID(child),
		WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	var rootRevision, childRevision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", root).Scan(&rootRevision)
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", child).Scan(&childRevision)

	// The raw row must not report its own revision as the chain's root revision.
	unprojected := issueToResponse(row, "WORK")
	if unprojected.LeadershipRootRevision != 0 || unprojected.LeaderAgentID != "" {
		t.Fatalf("raw subtask row carries unprojected leadership fields: root revision %d, leader %q",
			unprojected.LeadershipRootRevision, unprojected.LeaderAgentID)
	}

	h := *testHandler
	h.Bus = events.New()
	updates := make(chan events.Event, 4)
	h.Bus.Subscribe(protocol.EventIssueUpdated, func(event events.Event) { updates <- event })
	h.publishIssueChange(t.Context(), row, row.Status, "member", f.owner)

	var event events.Event
	select {
	case event = <-updates:
	default:
		t.Fatal("child change published no issue:updated event")
	}
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want map[string]any", event.Payload)
	}
	resp, ok := payload["issue"].(IssueResponse)
	if !ok {
		t.Fatalf("payload issue = %T, want IssueResponse", payload["issue"])
	}
	if resp.LeaderAgentID != f.agent || resp.LeaderExternalSessionID != f.a {
		t.Fatalf("child broadcast leader = %q/%q, want %q/%q", resp.LeaderAgentID, resp.LeaderExternalSessionID, f.agent, f.a)
	}
	if resp.LeadershipRootRevision != rootRevision {
		t.Fatalf("child broadcast root revision = %d, want %d (child revision %d)",
			resp.LeadershipRootRevision, rootRevision, childRevision)
	}
}
