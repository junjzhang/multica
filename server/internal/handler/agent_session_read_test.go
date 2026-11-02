package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func getAgentSessions(t *testing.T, userID, agentID string) (int, []AgentSessionSummaryResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	r := withURLParam(newRequestAs(userID, http.MethodGet, "/api/agents/"+agentID+"/sessions", nil), "id", agentID)
	testHandler.ListAgentSessions(w, r)
	var resp struct {
		Sessions []AgentSessionSummaryResponse `json:"sessions"`
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode sessions: %v", err)
		}
	}
	return w.Code, resp.Sessions
}

func getAgentIssueBinding(t *testing.T, req *http.Request, agentID string) (int, *AgentSessionBindingViewResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.GetAgentSessionBinding(w, withURLParam(req, "id", agentID))
	var resp struct {
		Binding *AgentSessionBindingViewResponse `json:"binding"`
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode binding: %v", err)
		}
	}
	return w.Code, resp.Binding
}

// TestListAgentSessionsReadAPI covers the authorized read of every registered
// session, including the two states that make a target undeliverable.
func TestListAgentSessionsReadAPI(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, firstSession, ownerID := externalAgentFixture(t, "Read Sessions", "daemon-read")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	secondSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "read-host-" + suffix,
		"session_id":    "read-session-" + suffix,
		"display_name":  "Second read session",
		"daemon_id":     "daemon-read-2",
	})

	code, sessions := getAgentSessions(t, testUserID, agentID)
	if code != http.StatusOK {
		t.Fatalf("owner list sessions status = %d, want 200", code)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	byID := map[string]AgentSessionSummaryResponse{}
	for _, session := range sessions {
		byID[session.ID] = session
	}
	if got := byID[firstSession]; got.ID == "" || !got.Deliverable || got.UnavailableReason != "" {
		t.Fatalf("first session = %+v, want deliverable", got)
	}
	if got := byID[secondSession]; got.ID == "" || got.DisplayName != "Second read session" || !got.Deliverable {
		t.Fatalf("second session = %+v, want the registered second session", got)
	}

	// A plain member cannot enumerate another agent's sessions.
	plain := dbfx.User(t, "Read Sessions Plain", "read-sessions-plain-"+suffix+"@test.local")
	dbfx.Member(t, testWorkspaceID, plain, "member")
	if code, _ := getAgentSessions(t, plain, agentID); code != http.StatusForbidden {
		t.Fatalf("plain member status = %d, want 403", code)
	}

	// An agent in another workspace is not visible through this workspace's
	// headers, even to the workspace owner.
	otherWorkspace := dbfx.Workspace(t, "Read Sessions Other Workspace", "read-sessions-other-ws-"+suffix)
	otherWorkspaceAgent := dbfx.Agent(t, "Read Sessions Foreign Agent", "", testutil.Cols{
		"workspace_id":    otherWorkspace,
		"owner_id":        ownerID,
		"execution_kind":  "external",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	if code, _ := getAgentSessions(t, testUserID, otherWorkspaceAgent); code != http.StatusNotFound {
		t.Fatalf("foreign workspace agent status = %d, want 404", code)
	}

	// The agent's own credential may read itself but not a sibling identity.
	otherAgent, _, _ := externalAgentFixture(t, "Read Sessions Other", "daemon-read-other")
	otherReq := withURLParam(newRequestAs(ownerID, http.MethodGet, "/api/agents/"+agentID+"/sessions", nil), "id", agentID)
	otherReq.Header.Set("X-Actor-Source", "external_agent")
	otherReq.Header.Set("X-Agent-ID", otherAgent)
	w := httptest.NewRecorder()
	testHandler.ListAgentSessions(w, otherReq)
	if w.Code != http.StatusForbidden {
		t.Fatalf("other agent credential status = %d, want 403", w.Code)
	}

	selfReq := withURLParam(newRequestAs(ownerID, http.MethodGet, "/api/agents/"+agentID+"/sessions", nil), "id", agentID)
	selfReq.Header.Set("X-Actor-Source", "external_agent")
	selfReq.Header.Set("X-Agent-ID", agentID)
	w = httptest.NewRecorder()
	testHandler.ListAgentSessions(w, selfReq)
	if w.Code != http.StatusOK {
		t.Fatalf("self credential status = %d, want 200", w.Code)
	}

	// Archived agents keep their rows but no session is a valid future target.
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, agentID)
	code, sessions = getAgentSessions(t, testUserID, agentID)
	if code != http.StatusOK || len(sessions) != 2 {
		t.Fatalf("archived agent list = %d/%d, want 200/2", code, len(sessions))
	}
	for _, session := range sessions {
		if session.Deliverable || session.UnavailableReason != "agent_archived" {
			t.Fatalf("archived session = %+v, want deliverable=false agent_archived", session)
		}
	}

	// A session whose owner left the workspace is equally explicit.
	leftOwner := dbfx.User(t, "Read Left Owner", "read-left-owner-"+suffix+"@test.local")
	leftAgent := dbfx.Agent(t, "Read Left Agent", "", testutil.Cols{
		"owner_id":        leftOwner,
		"execution_kind":  "external",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	leftSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": leftOwner,
		"agent_id":      leftAgent,
		"provider":      "paseo",
		"host_id":       "left-host-" + suffix,
		"session_id":    "left-session-" + suffix,
		"display_name":  "Left session",
		"daemon_id":     "daemon-left",
	})
	code, sessions = getAgentSessions(t, testUserID, leftAgent)
	if code != http.StatusOK || len(sessions) != 1 || sessions[0].ID != leftSession {
		t.Fatalf("owner-left sessions = %d/%+v, want the session", code, sessions)
	}
	if sessions[0].Deliverable || sessions[0].UnavailableReason != "owner_not_member" {
		t.Fatalf("owner-left session = %+v, want owner_not_member", sessions[0])
	}
}

// TestGetAgentSessionBindingReadAPI pins the exact (agent, issue) read:
// unbound is a null binding, a rebind is visible only for future resolution,
// and foreign agents/issues are refused.
func TestGetAgentSessionBindingReadAPI(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, firstSession, ownerID := externalAgentFixture(t, "Read Binding", "daemon-read-binding")
	issueID := dbfx.Issue(t, "read binding issue")

	req := func(userID, issue string) *http.Request {
		return newRequestAs(userID, http.MethodGet, "/api/agents/"+agentID+"/session-bindings?issue_id="+issue, nil)
	}
	code, binding := getAgentIssueBinding(t, req(testUserID, issueID), agentID)
	if code != http.StatusOK || binding != nil {
		t.Fatalf("unbound read = %d/%+v, want 200/null", code, binding)
	}

	bindExternalSession(t, agentID, firstSession, issueID)
	code, binding = getAgentIssueBinding(t, req(testUserID, issueID), agentID)
	if code != http.StatusOK || binding == nil {
		t.Fatalf("bound read = %d/%+v, want a binding", code, binding)
	}
	if binding.AgentID != agentID || binding.IssueID != issueID || binding.Session == nil || binding.Session.ID != firstSession {
		t.Fatalf("binding = %+v, want agent/issue/first session", binding)
	}
	if !binding.Deliverable {
		t.Fatalf("binding deliverable = false, want true: %+v", binding)
	}

	secondSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "binding-read-host-" + fmt.Sprint(time.Now().UnixNano()),
		"session_id":    "binding-read-session-" + fmt.Sprint(time.Now().UnixNano()),
		"display_name":  "Replacement",
		"daemon_id":     "daemon-replacement",
	})
	bindExternalSession(t, agentID, secondSession, issueID)
	code, binding = getAgentIssueBinding(t, req(testUserID, issueID), agentID)
	if code != http.StatusOK || binding == nil || binding.Session == nil || binding.Session.ID != secondSession {
		t.Fatalf("rebound read = %d/%+v, want the replacement", code, binding)
	}

	// A different issue in this workspace has no binding; a foreign workspace
	// issue is not silently treated as unbound.
	otherIssue := dbfx.Issue(t, "read binding other issue")
	code, binding = getAgentIssueBinding(t, req(testUserID, otherIssue), agentID)
	if code != http.StatusOK || binding != nil {
		t.Fatalf("other issue read = %d/%+v, want 200/null", code, binding)
	}
	code, _ = getAgentIssueBinding(t, req(testUserID, "00000000-0000-0000-0000-000000000001"), agentID)
	if code != http.StatusNotFound {
		t.Fatalf("foreign issue status = %d, want 404", code)
	}

	plain := dbfx.User(t, "Read Binding Plain", "read-binding-plain-"+fmt.Sprint(time.Now().UnixNano())+"@test.local")
	dbfx.Member(t, testWorkspaceID, plain, "member")
	code, _ = getAgentIssueBinding(t, req(plain, issueID), agentID)
	if code != http.StatusForbidden {
		t.Fatalf("plain member read status = %d, want 403", code)
	}
}

// TestIssueProjectionShowsCurrentBindingAndFrozenDeliveryTarget is the task
// card contract: each issue shows its own binding, a rebind changes only the
// current binding, and an old generic delivery keeps the target it froze.
func TestIssueProjectionShowsCurrentBindingAndFrozenDeliveryTarget(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionA, ownerID := externalAgentFixture(t, "Projection Binding", "daemon-projection")
	sessionB := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "projection-host-b-" + fmt.Sprint(time.Now().UnixNano()),
		"session_id":    "projection-session-b-" + fmt.Sprint(time.Now().UnixNano()),
		"display_name":  "Session B",
		"daemon_id":     "daemon-projection-b",
	})
	issueA := dbfx.Issue(t, "projection issue A")
	issueB := dbfx.Issue(t, "projection issue B")
	bindExternalSession(t, agentID, sessionA, issueA)
	bindExternalSession(t, agentID, sessionB, issueB)

	getIssue := func(id string) IssueResponse {
		w := httptest.NewRecorder()
		testHandler.GetIssue(w, withURLParam(newRequest(http.MethodGet, "/api/issues/"+id, nil), "id", id))
		if w.Code != http.StatusOK {
			t.Fatalf("get issue %s: %d %s", id, w.Code, w.Body.String())
		}
		var resp IssueResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode issue %s: %v", id, err)
		}
		return resp
	}
	assertBindingSession := func(resp IssueResponse, issueID, want string) {
		t.Helper()
		if resp.ExternalSessionBindings == nil || len(*resp.ExternalSessionBindings) != 1 {
			t.Fatalf("bindings = %+v, want one", resp.ExternalSessionBindings)
		}
		binding := (*resp.ExternalSessionBindings)[0]
		if binding.AgentID != agentID || binding.IssueID != issueID || binding.Session == nil || binding.Session.ID != want {
			t.Fatalf("binding = %+v, want agent %s issue %s session %s", binding, agentID, issueID, want)
		}
	}
	assertBindingSession(getIssue(issueA), issueA, sessionA)
	assertBindingSession(getIssue(issueB), issueB, sessionB)

	// Persist one generic delivery against issue A while session A is bound.
	generic, err := delivery.NewPostgres(testHandler.Queries).Enqueue(context.Background(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: testWorkspaceID},
		Source:          delivery.Source{Type: "issue_wakeup", Key: "projection-wakeup-" + fmt.Sprint(time.Now().UnixNano())},
		Target:          delivery.Target{SessionID: sessionA},
		Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
		NotificationKey: "projection:wakeup:" + fmt.Sprint(time.Now().UnixNano()),
		Kind:            "issue_wakeup",
		Refs:            delivery.Refs{IssueID: issueA},
		Payload:         json.RawMessage(`{"schema":"issue_wakeup.v1","issue_id":"` + issueA + `"}`),
	})
	if err != nil {
		t.Fatalf("enqueue generic delivery: %v", err)
	}
	bindExternalSession(t, agentID, sessionB, issueA)
	afterRebind := getIssue(issueA)
	assertBindingSession(afterRebind, issueA, sessionB)
	if afterRebind.MailboxDeliveries == nil || len(*afterRebind.MailboxDeliveries) != 1 {
		t.Fatalf("deliveries = %+v, want the generic row", afterRebind.MailboxDeliveries)
	}
	row := (*afterRebind.MailboxDeliveries)[0]
	if row.ID != generic.ID || row.CommentID != "" || row.Kind != "issue_wakeup" || row.SourceType != "issue_wakeup" {
		t.Fatalf("generic row = %+v, want kind issue_wakeup with no comment", row)
	}
	if row.TargetSession == nil || row.TargetSession.ID != sessionA {
		t.Fatalf("generic target = %+v, want frozen session A %s", row.TargetSession, sessionA)
	}
	if afterRebind.ExternalSessionBindings == nil || (*afterRebind.ExternalSessionBindings)[0].Session.ID != sessionB {
		t.Fatal("rebind changed the current binding but moved the old delivery target")
	}
}
