package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// registerExternalSession drives the production registration handler the way a
// host does: one POST per physical host session.
func registerExternalSession(t *testing.T, body map[string]any) (int, AgentResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.CreateExternalAgent(w, newRequest(http.MethodPost, "/api/agents/external", body))
	var resp AgentResponse
	if w.Code < 300 {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode registration response: %v", err)
		}
	}
	return w.Code, resp
}

// cleanupRegisteredExternalAgent removes the identity and sessions a
// registration test created. Without this, the LIMIT 1 lookups in older tests
// can select this external agent (no runtime) and fail by ordering.
func cleanupRegisteredExternalAgent(t *testing.T, agentID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM external_agent_session WHERE agent_id = $1`, agentID)
		_, _ = testPool.Exec(ctx, `DELETE FROM external_agent_token WHERE agent_id = $1`, agentID)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID)
	})
}

func externalSessionBody(host, sessionID, displayName, daemonID string) map[string]any {
	return map[string]any{
		"provider": "paseo", "host_id": host, "session_id": sessionID,
		"display_name": displayName, "daemon_id": daemonID,
		"url": "paseo://h/" + host + "/agent/" + sessionID,
	}
}

func storedSessionProfile(t *testing.T, agentID, hostID string) (displayName, daemonID string) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(), `
		SELECT display_name, daemon_id FROM external_agent_session
		WHERE agent_id = $1 AND host_id = $2
	`, agentID, hostID).Scan(&displayName, &daemonID); err != nil {
		t.Fatalf("read stored session profile: %v", err)
	}
	return displayName, daemonID
}

// Re-registering one physical host session must update exactly that session.
// The first regression: the profile update was scoped by agent_id, so a host
// re-running registration for its second session renamed and re-pointed its
// first session's daemon as well.
func TestRepeatRegistrationUpdatesOnlyTheNamedSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemon1, daemon2 := "rereg-d1-"+suffix, "rereg-d2-"+suffix
	dbfx.Runtime(t, "Reregistration host 1", testutil.Cols{"daemon_id": daemon1})
	dbfx.Runtime(t, "Reregistration host 2", testutil.Cols{"daemon_id": daemon2})
	host1, host2 := "rereg-h1-"+suffix, "rereg-h2-"+suffix

	code, created := registerExternalSession(t, map[string]any{
		"name":             "Reregistration " + suffix,
		"external_session": externalSessionBody(host1, "s1", "S1", daemon1),
	})
	if code != http.StatusCreated {
		t.Fatalf("first registration status = %d, want 201", code)
	}
	cleanupRegisteredExternalAgent(t, created.ID)
	code, second := registerExternalSession(t, map[string]any{
		"agent_id":         created.ID,
		"external_session": externalSessionBody(host2, "s2", "S2", daemon2),
	})
	if code != http.StatusCreated {
		t.Fatalf("second session registration status = %d, want 201", code)
	}
	if second.ExternalSession == nil || second.ExternalSession.HostID != host2 ||
		second.ExternalSession.SessionID != "s2" || second.ExternalSession.DaemonID != daemon2 {
		t.Fatalf("second registration response session = %+v, want %s/%s on %s", second.ExternalSession, host2, "s2", daemon2)
	}

	// The host idempotently re-registers S2, now with a renamed profile.
	code, repeated := registerExternalSession(t, map[string]any{
		"agent_id":         created.ID,
		"external_session": externalSessionBody(host2, "s2", "S2-renamed", daemon2),
	})
	if code != http.StatusOK {
		t.Fatalf("repeat registration status = %d, want 200", code)
	}
	if repeated.ExternalSession == nil || repeated.ExternalSession.HostID != host2 ||
		repeated.ExternalSession.SessionID != "s2" || repeated.ExternalSession.DisplayName != "S2-renamed" ||
		repeated.ExternalSession.DaemonID != daemon2 {
		t.Fatalf("repeat registration response session = %+v, want the renamed %s/s2", repeated.ExternalSession, host2)
	}

	if name, daemon := storedSessionProfile(t, created.ID, host1); name != "S1" || daemon != daemon1 {
		t.Fatalf("sibling session S1 = %s/%s, want S1/%s: re-registering S2 must not touch it", name, daemon, daemon1)
	}
	if name, daemon := storedSessionProfile(t, created.ID, host2); name != "S2-renamed" || daemon != daemon2 {
		t.Fatalf("session S2 = %s/%s, want S2-renamed/%s", name, daemon, daemon2)
	}
	if n := countRows(t, `SELECT count(*) FROM external_agent_session WHERE agent_id = $1`, created.ID); n != 2 {
		t.Fatalf("session rows = %d, want 2", n)
	}
}

// A physical session identity cannot be moved to another agent by naming that
// agent during re-registration. The explicit agent_id conflicts with the
// existing ownership and nothing is written.
func TestRepeatRegistrationRejectsDifferentAgentID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemon1, daemon2 := "rereg-owner1-"+suffix, "rereg-owner2-"+suffix
	dbfx.Runtime(t, "Reregistration owner 1", testutil.Cols{"daemon_id": daemon1})
	dbfx.Runtime(t, "Reregistration owner 2", testutil.Cols{"daemon_id": daemon2})
	host1, host2 := "rereg-owner-h1-"+suffix, "rereg-owner-h2-"+suffix

	code, first := registerExternalSession(t, map[string]any{
		"name":             "Reregistration owner A " + suffix,
		"external_session": externalSessionBody(host1, "s1", "S1", daemon1),
	})
	if code != http.StatusCreated {
		t.Fatalf("first registration status = %d, want 201", code)
	}
	cleanupRegisteredExternalAgent(t, first.ID)
	code, other := registerExternalSession(t, map[string]any{
		"name":             "Reregistration owner B " + suffix,
		"external_session": externalSessionBody(host2, "s2", "S2", daemon2),
	})
	if code != http.StatusCreated {
		t.Fatalf("other agent registration status = %d, want 201", code)
	}

	code, _ = registerExternalSession(t, map[string]any{
		"agent_id":         other.ID,
		"external_session": externalSessionBody(host1, "s1", "Stolen", daemon2),
	})
	if code != http.StatusConflict {
		t.Fatalf("re-registration under the wrong agent_id = %d, want 409", code)
	}
	if name, daemon := storedSessionProfile(t, first.ID, host1); name != "S1" || daemon != daemon1 {
		t.Fatalf("session after rejected registration = %s/%s, want S1/%s: a conflict must not write", name, daemon, daemon1)
	}
	if n := countRows(t, `SELECT count(*) FROM external_agent_session WHERE agent_id = $1`, other.ID); n != 1 {
		t.Fatalf("other agent session rows = %d, want 1", n)
	}
}

// externalAgentRequest stamps the headers the auth middleware sets for a mea_
// credential, so the handler sees a concrete external agent as the actor.
func externalAgentRequest(t *testing.T, agentID, method, path string, body any) *http.Request {
	t.Helper()
	req := newRequest(method, path, body)
	req.Header.Set("X-Actor-Source", "external_agent")
	req.Header.Set("X-Agent-ID", agentID)
	// A session credential acts as exactly one registered session. The real
	// auth middleware stamps this from the token; this helper models the same
	// request shape for handlers that are exercised directly.
	req.Header.Set("X-Actor-Session-ID", oldestExternalSessionID(t, agentID))
	return req
}

// oldestExternalSessionID is the session a session-bound credential for this
// agent would name when the test does not care which one: the oldest live
// session, matching the auth path's single-session rule.
func oldestExternalSessionID(t *testing.T, agentID string) string {
	t.Helper()
	var sessionID string
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM external_agent_session
		WHERE agent_id = $1 AND revoked_at IS NULL ORDER BY created_at ASC, id ASC LIMIT 1`, agentID).Scan(&sessionID); err != nil {
		t.Fatalf("load oldest external session for %s: %v", agentID, err)
	}
	return sessionID
}

// Editing a comment whose sender has since rebound must replay the original
// delivery. Source.SessionID is the sender's route when the notification was
// created, not part of its logical identity: before the fix the edit returned
// 500 because the re-resolved source session no longer matched.
func TestCommentEditReplayAfterSenderRebindKeepsOriginalRoute(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	recipient, recipientSession, _ := externalAgentFixture(t, "Replay Recipient", "daemon-replay-r")
	sender, senderSession, senderOwner := externalAgentFixture(t, "Replay Sender", "daemon-replay-s")
	secondSenderSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": senderOwner,
		"agent_id":      sender,
		"provider":      "paseo",
		"host_id":       "replay-second-host-" + sender,
		"session_id":    "replay-second-session-" + sender,
		"display_name":  "Replay Sender 2",
		"daemon_id":     "daemon-replay-s2",
	})
	issueID := dbfx.Issue(t, "sender rebind comment replay")
	bindExternalSession(t, recipient, recipientSession, issueID)
	bindExternalSession(t, sender, senderSession, issueID)

	content := fmt.Sprintf("[@Recipient](mention://agent/%s) please review", recipient)
	w := httptest.NewRecorder()
	r := externalAgentRequest(t, sender, http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create comment status = %d: %s", w.Code, w.Body.String())
	}
	var created CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode created comment: %v", err)
	}
	outcome := findCommentOutcome(t, created.TriggerOutcomes, recipient)
	if outcome.Status != DispatchQueued || outcome.DispatchKind != "mailbox" {
		t.Fatalf("create outcome = %+v, want queued/mailbox", outcome)
	}

	var deliveryID, deliveryTarget, deliverySource string
	if err := testPool.QueryRow(t.Context(), `
		SELECT id, target_session_id, source_session_id FROM agent_mailbox_message
		WHERE comment_id = $1 AND recipient_agent_id = $2
	`, created.ID, recipient).Scan(&deliveryID, &deliveryTarget, &deliverySource); err != nil {
		t.Fatalf("read first delivery: %v", err)
	}
	if deliveryTarget != recipientSession || deliverySource != senderSession {
		t.Fatalf("first delivery target/source = %s/%s, want %s/%s", deliveryTarget, deliverySource, recipientSession, senderSession)
	}

	// The sender explicitly moves its own target on this issue to a second
	// physical session.
	if _, err := sessionroute.Bind(context.Background(), testHandler.Queries, sessionroute.BindRequest{
		WorkspaceID: testWorkspaceID,
		AgentID:     sender,
		IssueID:     issueID,
		SessionID:   secondSenderSession,
	}); err != nil {
		t.Fatalf("rebind sender: %v", err)
	}

	// A real content edit through the production handler re-plans the same
	// mention and re-enqueues the same logical notification.
	w = httptest.NewRecorder()
	r = externalAgentRequest(t, sender, http.MethodPut, "/api/comments/"+created.ID, map[string]any{
		"content":           content + " edited",
		"expected_revision": created.Revision,
	})
	r = withURLParam(r, "commentId", created.ID)
	testHandler.UpdateComment(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("edit after sender rebind status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var edited CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&edited); err != nil {
		t.Fatalf("decode edited comment: %v", err)
	}
	outcome = findCommentOutcome(t, edited.TriggerOutcomes, recipient)
	if outcome.Status != DispatchQueued || outcome.DispatchKind != "mailbox" {
		t.Fatalf("edit outcome = %+v, want queued/mailbox replay", outcome)
	}

	var replayedID, replayedTarget, replayedSource string
	if err := testPool.QueryRow(t.Context(), `
		SELECT id, target_session_id, source_session_id FROM agent_mailbox_message
		WHERE comment_id = $1 AND recipient_agent_id = $2
	`, created.ID, recipient).Scan(&replayedID, &replayedTarget, &replayedSource); err != nil {
		t.Fatalf("read replayed delivery: %v", err)
	}
	if replayedID != deliveryID || replayedTarget != deliveryTarget || replayedSource != deliverySource {
		t.Fatalf("replayed delivery = %s/%s/%s, want the original %s/%s/%s",
			replayedID, replayedTarget, replayedSource, deliveryID, deliveryTarget, deliverySource)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE comment_id = $1 AND recipient_agent_id = $2`, created.ID, recipient); n != 1 {
		t.Fatalf("delivery rows = %d, want 1", n)
	}
}
