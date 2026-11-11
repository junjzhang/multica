package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type handoffAPIFixture struct {
	h                          *Handler
	agent, a, b, tokenA, squad string
}

func newHandoffAPIFixture(t *testing.T) handoffAPIFixture {
	t.Helper()
	h := templatesEnabledHandler(t)
	suffix := uuidToString(dbid.NewV7())
	daemon, host := "handoff-daemon-"+suffix, "handoff-host-"+suffix
	// A runtime represents the daemon transport; no HTTP runtime creation endpoint exists.
	runtime := dbfx.Runtime(t, "Handoff test daemon", testutil.Cols{"daemon_id": daemon})
	var managed AgentResponse
	testutil.Call(t, h.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{"name": "Handoff default " + suffix, "runtime_id": runtime, "visibility": "workspace"})).Want(201).JSON(&managed)
	dbfx.Cleanup(t, "DELETE FROM agent WHERE id=$1", managed.ID)
	code, first, body := registerExternal(t, h, templateBody("Handoff "+suffix, host, "a-"+suffix, "Session A", daemon))
	if code != 201 {
		t.Fatalf("register first: %d %s", code, body)
	}
	cleanupExternalAgent(t, first.ID)
	code, second, body := registerExternal(t, h, templateBody("Handoff "+suffix, host, "b-"+suffix, "Session B", daemon))
	if code != 201 || first.ID != second.ID || first.ExternalSession == nil || second.ExternalSession == nil {
		t.Fatalf("register second: %d %s", code, body)
	}
	var token createExternalAgentTokenResponse
	testutil.Call(t, h.CreateExternalAgentToken, withURLParam(newRequest(http.MethodPost, "/api/agents/"+first.ID+"/tokens", map[string]any{"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "external_session_id": first.ExternalSession.ID}), "id", first.ID)).Want(201).JSON(&token)
	squad := createSquadAs(t, "", "Handoff squad "+suffix, managed.ID)
	testutil.Call(t, h.AddSquadMember, squadScopeReq("", http.MethodPost, "/api/squads/"+squad.ID+"/members", map[string]any{"member_type": "agent", "member_id": first.ID}, map[string]string{"id": squad.ID})).Want(201)
	return handoffAPIFixture{h: h, agent: first.ID, a: first.ExternalSession.ID, b: second.ExternalSession.ID, tokenA: token.Token, squad: squad.ID}
}

func (f handoffAPIFixture) root(t *testing.T) IssueResponse {
	t.Helper()
	rec := serveAuthenticated(t, f.tokenA, http.HandlerFunc(f.h.CreateIssue), http.MethodPost, "/api/issues", map[string]any{"title": "Handoff root " + uuidToString(dbid.NewV7()), "assignee_type": "squad", "assignee_id": f.squad}, nil, nil)
	if rec.Code != 201 {
		t.Fatalf("create root: %d %s", rec.Code, rec.Body.String())
	}
	var out IssueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	cleanupDelegatedIssue(t, out.ID)
	return out
}

func TestHandoffExplicitAndLegacyAPI(t *testing.T) {
	f := newHandoffAPIFixture(t)
	t.Run("child changes root leader", func(t *testing.T) {
		root := f.root(t)
		var child IssueResponse
		testutil.Call(t, f.h.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "Handoff child", "status": "backlog", "parent_issue_id": root.ID, "leadership_root_issue_id": root.ID, "assignee_type": "agent", "assignee_id": f.agent, "recipient_session_id": f.a})).Want(201).JSON(&child)
		cleanupDelegatedIssue(t, child.ID)
		var current IssueResponse
		testutil.Call(t, f.h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+root.ID, nil), "id", root.ID)).Want(200).JSON(&current)
		req := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedRevision: current.LeadershipRevision, ExpectedIssueRevision: current.Revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: f.agent, SessionID: f.b, Reason: "Recover leader"}
		testutil.Call(t, f.h.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, "/api/issues/"+child.ID+"/task-leader/handoff", req), "id", child.ID)).Want(200)
		testutil.Call(t, f.h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+root.ID, nil), "id", root.ID)).Want(200).JSON(&current)
		if current.LeaderExternalSessionID != f.b || current.LeadershipRevision != req.ExpectedRevision+1 {
			t.Fatalf("leader not advanced: %+v", current)
		}
		var after IssueResponse
		testutil.Call(t, f.h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+child.ID, nil), "id", child.ID)).Want(200).JSON(&after)
		if after.AssigneeID == nil || *after.AssigneeID != f.agent {
			t.Fatalf("child assignee changed: %+v", after)
		}
		if n := dbfx.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='leadership_handoff'", root.ID); n != 1 {
			t.Fatalf("root activity count=%d", n)
		}
		testutil.Call(t, f.h.HandoffIssueExecutor, withURLParam(newRequest(http.MethodPost, "/api/issues/"+root.ID+"/executor/handoff", req), "id", root.ID)).Want(409)
	})
	t.Run("legacy leader", func(t *testing.T) {
		root := f.root(t)
		req := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedRevision: root.LeadershipRevision, ExpectedIssueRevision: root.Revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: f.agent, SessionID: f.b, Reason: "Legacy leader"}
		rec := testutil.Call(t, f.h.LegacyIssueHandoff, withURLParam(newRequest(http.MethodPost, "/api/issues/"+root.ID+"/leadership/handoff", req), "id", root.ID)).Want(200)
		if rec.Header().Get("Deprecation") != "true" {
			t.Fatal("missing deprecation header")
		}
	})
	t.Run("executor and legacy", func(t *testing.T) {
		for _, legacy := range []bool{false, true} {
			var issue IssueResponse
			testutil.Call(t, f.h.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "Handoff executor " + uuidToString(dbid.NewV7()), "status": "backlog", "assignee_type": "agent", "assignee_id": f.agent, "recipient_session_id": f.a})).Want(201).JSON(&issue)
			cleanupDelegatedIssue(t, issue.ID)
			contractCall(t, issue.ID, definition(0), "").Want(200)
			testutil.Call(t, f.h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issue.ID, nil), "id", issue.ID)).Want(200).JSON(&issue)
			req := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedIssueRevision: issue.Revision, PreviousAgentID: f.agent, PreviousSessionID: f.a, AgentID: f.agent, SessionID: f.b, Reason: "Recover executor"}
			testutil.Call(t, f.h.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue.ID+"/task-leader/handoff", req), "id", issue.ID)).Want(409)
			handler := f.h.HandoffIssueExecutor
			if legacy {
				handler = f.h.LegacyIssueHandoff
			}
			for range 2 {
				testutil.Call(t, handler, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue.ID+"/handoff", req), "id", issue.ID)).Want(200)
			}
			var view ContractView
			testutil.Call(t, f.h.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/contract", nil), "id", issue.ID)).Want(200).JSON(&view)
			if !view.Stale {
				t.Fatal("executor handoff did not stale the contract")
			}
			contractCall(t, issue.ID, definition(1), "").Want(200)
			submit := contractRequest{Action: "submit", Version: 2, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Old executor must not write"}
			authenticatedContractCall(t, issue.ID, submit, f.tokenA).Want(409)
			if n := dbfx.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='executor_handoff'", issue.ID); n != 1 {
				t.Fatalf("executor activity count=%d", n)
			}
		}
	})
}
