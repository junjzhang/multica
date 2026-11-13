package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// postExplicitMentionOutcome posts one member comment that explicitly mentions the
// agent and returns its trigger outcome for that agent.
func postExplicitMentionOutcome(t *testing.T, issueID, agentID, content string) CommentTriggerOutcome {
	t.Helper()
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: %d %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	return findCommentOutcome(t, resp.TriggerOutcomes, agentID)
}

// A comment handoff resolves the explicit (agent, issue) binding and freezes
// that session on the delivery. Rebinding changes only later notifications;
// an unbound agent fails the target instead of guessing a session.
func TestCommentMentionFreezesBoundSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, ownerID := externalAgentFixture(t, "Binding Target", "daemon-binding")
	issueID := dbfx.Issue(t, "binding target")
	bindExternalSession(t, agentID, sessionID, issueID)

	outcome := postExplicitMentionOutcome(t, issueID, agentID, fmt.Sprintf("[@Target](mention://agent/%s) take this", agentID))
	if outcome.Status != DispatchQueued || outcome.DispatchKind != "mailbox" {
		t.Fatalf("outcome = %+v, want queued/mailbox", outcome)
	}
	var target, notificationKey string
	if err := testPool.QueryRow(context.Background(), `
		SELECT target_session_id, notification_key FROM agent_mailbox_message
		WHERE issue_id = $1 AND recipient_agent_id = $2
		ORDER BY created_at DESC LIMIT 1`, issueID, agentID).Scan(&target, &notificationKey); err != nil {
		t.Fatalf("read bound delivery: %v", err)
	}
	if target != sessionID {
		t.Fatalf("delivery target = %s, want the bound session %s", target, sessionID)
	}
	if notificationKey == "" {
		t.Fatal("delivery has no notification idempotency key")
	}
	firstID := notificationKey

	// A second session for the same agent, then an explicit rebind.
	secondSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "host-rebind-" + agentID,
		"session_id":    "session-rebind-" + agentID,
		"display_name":  "Rebind Target",
		"daemon_id":     "daemon-rebind",
	})
	bindExternalSession(t, agentID, secondSession, issueID)
	if outcome := postExplicitMentionOutcome(t, issueID, agentID, fmt.Sprintf("[@Target](mention://agent/%s) again", agentID)); outcome.Status != DispatchQueued {
		t.Fatalf("second outcome = %+v, want queued", outcome)
	}

	var firstTarget, reboundTarget string
	if err := testPool.QueryRow(context.Background(), `
		SELECT target_session_id FROM agent_mailbox_message
		WHERE issue_id = $1 AND recipient_agent_id = $2 AND notification_key = $3`,
		issueID, agentID, firstID).Scan(&firstTarget); err != nil {
		t.Fatalf("reload first delivery: %v", err)
	}
	if firstTarget != sessionID {
		t.Fatalf("first delivery target = %s, want the frozen %s", firstTarget, sessionID)
	}
	if err := testPool.QueryRow(context.Background(), `
		SELECT target_session_id FROM agent_mailbox_message
		WHERE issue_id = $1 AND recipient_agent_id = $2 AND notification_key <> $3
		ORDER BY created_at DESC LIMIT 1`, issueID, agentID, firstID).Scan(&reboundTarget); err != nil {
		t.Fatalf("read rebound delivery: %v", err)
	}
	if reboundTarget != secondSession {
		t.Fatalf("rebound delivery target = %s, want the replacement %s", reboundTarget, secondSession)
	}

	// An external agent with no binding on this issue is blocked, and no row
	// is invented for it.
	unboundAgent, _, _ := externalAgentFixture(t, "Unbound Target", "daemon-unbound")
	if outcome := postExplicitMentionOutcome(t, issueID, unboundAgent, fmt.Sprintf("[@Unbound](mention://agent/%s) hello", unboundAgent)); outcome.Status != DispatchBlocked || outcome.ReasonCode != ReasonTargetUnavailable {
		t.Fatalf("unbound outcome = %+v, want blocked/target_unavailable", outcome)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id = $1 AND recipient_agent_id = $2`, issueID, unboundAgent); n != 0 {
		t.Fatalf("unbound agent rows = %d, want none", n)
	}
}

// TestBindAgentSessionAPI pins the explicit takeover surface: an authorized
// caller can point a future notification at a replacement session, a foreign
// session is refused, and a plain member who does not own the agent cannot
// rebind it.
func TestBindAgentSessionAPI(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, _ := externalAgentFixture(t, "API Binding Target", "daemon-api-binding")
	issueID := dbfx.Issue(t, "api binding issue")

	bind := func(userID, agent, session, issue string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := withURLParam(newRequestAs(userID, http.MethodPost, "/api/agents/"+agent+"/session-bindings", map[string]any{
			"issue_id":   issue,
			"session_id": session,
		}), "id", agent)
		testHandler.BindAgentSession(w, r)
		return w
	}

	w := bind(testUserID, agentID, sessionID, issueID)
	if w.Code != http.StatusCreated {
		t.Fatalf("bind status = %d: %s", w.Code, w.Body.String())
	}
	var binding AgentSessionBindingResponse
	if err := json.NewDecoder(w.Body).Decode(&binding); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if binding.AgentID != agentID || binding.IssueID != issueID || binding.SessionID != sessionID {
		t.Fatalf("binding = %+v, want the requested target", binding)
	}

	// A session of another agent cannot be bound to this one.
	otherAgent, otherSession, _ := externalAgentFixture(t, "API Binding Other", "daemon-api-binding")
	if w := bind(testUserID, agentID, otherSession, issueID); w.Code != http.StatusBadRequest {
		t.Fatalf("foreign session status = %d, want 400: %s", w.Code, w.Body.String())
	}
	_ = otherAgent

	// A plain member who neither owns the agent nor administers the workspace
	// is refused before any write.
	plainMember := dbfx.User(t, "Plain Binding Member", fmt.Sprintf("plain-binding-%d@test.local", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, plainMember, "member")
	if w := bind(plainMember, agentID, sessionID, issueID); w.Code != http.StatusForbidden {
		t.Fatalf("plain member status = %d, want 403: %s", w.Code, w.Body.String())
	}
}
