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

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// externalAgentFixture registers one external identity the same way the
// production registration path does, without going through HTTP.
func externalAgentFixture(t *testing.T, name, daemonID string) (agentID, sessionID, ownerID string) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	handle := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	ownerID = dbfx.User(t, name+" Owner", handle+"-"+suffix+"@external.test")
	dbfx.Member(t, testWorkspaceID, ownerID, "member")
	agentID = dbfx.Agent(t, name+" "+suffix, "", testutil.Cols{
		"owner_id":        ownerID,
		"execution_kind":  "external",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	sessionID = dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "host-" + suffix,
		"session_id":    "session-" + suffix,
		"display_name":  name,
		"daemon_id":     daemonID,
	})
	// Workspace-invocable so any member (including the test user) may address
	// the agent explicitly.
	dbfx.Exec(t, `
		INSERT INTO agent_invocation_target (agent_id, target_type, target_id)
		VALUES ($1, 'workspace', $2)
		ON CONFLICT (agent_id, target_type, target_id) DO NOTHING
	`, agentID, testWorkspaceID)
	return agentID, sessionID, ownerID
}

func externalTokenFixture(t *testing.T, agentID, ownerID string, expiresAt time.Time) string {
	t.Helper()
	raw, err := auth.GenerateExternalAgentToken()
	if err != nil {
		t.Fatalf("generate external token: %v", err)
	}
	dbfx.Insert(t, "external_agent_token", testutil.Cols{
		"agent_id":      agentID,
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"token_hash":    auth.HashToken(raw),
		"expires_at":    expiresAt,
	})
	return raw
}

// serveExternalAuth runs one request through the real Auth middleware and
// returns the response plus the headers the authenticated request carried.
func serveExternalAuth(t *testing.T, raw string, mutate func(*http.Request)) (*httptest.ResponseRecorder, http.Header) {
	t.Helper()
	var captured http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	middleware.Auth(testHandler.Queries, nil, nil, nil)(next).ServeHTTP(w, req)
	return w, captured
}

// TestExternalAgentTokenAuthLifecycle covers the credential boundary the
// contract calls out: a valid token authenticates one concrete external agent
// (no task), and revocation, expiry, archive or membership loss invalidates it.
func TestExternalAgentTokenAuthLifecycle(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, ownerID := externalAgentFixture(t, "Auth Lifecycle", "daemon-auth")

	t.Run("valid", func(t *testing.T) {
		raw := externalTokenFixture(t, agentID, ownerID, time.Now().Add(time.Hour))
		w, headers := serveExternalAuth(t, raw, func(r *http.Request) {
			// Forged identity headers must be replaced by the token's own row.
			r.Header.Set("X-Agent-ID", "00000000-0000-0000-0000-000000000001")
			r.Header.Set("X-Task-ID", "00000000-0000-0000-0000-000000000002")
			r.Header.Set("X-Actor-Source", "task_token")
		})
		if w.Code != http.StatusOK {
			t.Fatalf("valid token status = %d: %s", w.Code, w.Body.String())
		}
		if got := headers.Get("X-Agent-ID"); got != agentID {
			t.Fatalf("X-Agent-ID = %q, want token agent %s", got, agentID)
		}
		if got := headers.Get("X-Task-ID"); got != "" {
			t.Fatalf("X-Task-ID = %q, want empty (external identities have no task)", got)
		}
		if got := headers.Get("X-Actor-Source"); got != "external_agent" {
			t.Fatalf("X-Actor-Source = %q, want external_agent", got)
		}
		if got := headers.Get("X-User-ID"); got != ownerID {
			t.Fatalf("X-User-ID = %q, want owner %s", got, ownerID)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		raw := externalTokenFixture(t, agentID, ownerID, time.Now().Add(time.Hour))
		dbfx.Exec(t, `UPDATE external_agent_token SET revoked_at = now() WHERE token_hash = $1`, auth.HashToken(raw))
		w, _ := serveExternalAuth(t, raw, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("revoked token status = %d, want 401", w.Code)
		}
	})

	t.Run("expired", func(t *testing.T) {
		raw := externalTokenFixture(t, agentID, ownerID, time.Now().Add(-time.Minute))
		w, _ := serveExternalAuth(t, raw, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expired token status = %d, want 401", w.Code)
		}
	})

	t.Run("archived agent", func(t *testing.T) {
		archAgent, _, archOwner := externalAgentFixture(t, "Auth Archived", "daemon-auth")
		raw := externalTokenFixture(t, archAgent, archOwner, time.Now().Add(time.Hour))
		dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, archAgent)
		w, _ := serveExternalAuth(t, raw, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("archived agent token status = %d, want 401", w.Code)
		}
	})

	t.Run("membership loss", func(t *testing.T) {
		lossAgent, _, lossOwner := externalAgentFixture(t, "Auth Membership", "daemon-auth")
		raw := externalTokenFixture(t, lossAgent, lossOwner, time.Now().Add(time.Hour))
		dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, lossOwner)
		w, _ := serveExternalAuth(t, raw, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("membership-loss token status = %d, want 401", w.Code)
		}
	})

	t.Run("cross workspace selector", func(t *testing.T) {
		raw := externalTokenFixture(t, agentID, ownerID, time.Now().Add(time.Hour))
		otherWorkspace := dbfx.Workspace(t, "Other Workspace", fmt.Sprintf("other-ws-%d", time.Now().UnixNano()))
		w, _ := serveExternalAuth(t, raw, func(r *http.Request) {
			r.Header.Set("X-Workspace-ID", otherWorkspace)
		})
		if w.Code != http.StatusForbidden {
			t.Fatalf("cross-workspace selector status = %d, want 403: %s", w.Code, w.Body.String())
		}
	})
}

// TestExternalAgentsAreDistinctAuthors proves two external sessions author
// comments as different agents and that an explicit mention routes to the
// target's mailbox, not to a managed run.
func TestExternalAgentsAreDistinctAuthors(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentA, _, _ := externalAgentFixture(t, "External Author A", "daemon-authors")
	agentB, _, _ := externalAgentFixture(t, "External Author B", "daemon-authors")
	issueID := dbfx.Issue(t, "external authors")

	postAs := func(agentID, content string) CommentResponse {
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
		r.Header.Set("X-Actor-Source", "external_agent")
		r.Header.Set("X-Agent-ID", agentID)
		r = withURLParam(r, "id", issueID)
		testHandler.CreateComment(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateComment as %s: %d %s", agentID, w.Code, w.Body.String())
		}
		var resp CommentResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode comment: %v", err)
		}
		return resp
	}

	first := postAs(agentA, "first external author")
	if first.AuthorType != "agent" || first.AuthorID != agentA {
		t.Fatalf("first author = %s/%s, want agent/%s", first.AuthorType, first.AuthorID, agentA)
	}
	mention := fmt.Sprintf("[@B](mention://agent/%s) please review", agentB)
	second := postAs(agentA, mention)
	if second.AuthorID != agentA {
		t.Fatalf("mention comment author = %s, want %s", second.AuthorID, agentA)
	}
	outcome := findCommentOutcome(t, second.TriggerOutcomes, agentB)
	if outcome.Status != DispatchQueued || outcome.DispatchKind != "mailbox" {
		t.Fatalf("external mention outcome = %+v, want queued/mailbox", outcome)
	}
	// No managed run may be created for an external target.
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentB); n != 0 {
		t.Fatalf("external agent has %d managed tasks, want 0", n)
	}
	var commentID, sourceAgent string
	if err := testPool.QueryRow(context.Background(), `
		SELECT comment_id, recipient_agent_id FROM agent_mailbox_message
		WHERE issue_id = $1 AND recipient_agent_id = $2
	`, issueID, agentB).Scan(&commentID, &sourceAgent); err != nil {
		t.Fatalf("read mailbox message: %v", err)
	}
	if commentID != second.ID {
		t.Fatalf("mailbox comment_id = %s, want %s", commentID, second.ID)
	}
	var sourceSessionAgent string
	if err := testPool.QueryRow(context.Background(), `
		SELECT s.agent_id FROM agent_mailbox_message m
		JOIN external_agent_session s ON s.id = m.source_session_id
		WHERE m.issue_id = $1 AND m.recipient_agent_id = $2
	`, issueID, agentB).Scan(&sourceSessionAgent); err != nil {
		t.Fatalf("read source session: %v", err)
	}
	if sourceSessionAgent != agentA {
		t.Fatalf("source session agent = %s, want sender %s", sourceSessionAgent, agentA)
	}

	reply := postAs(agentB, "external reply")
	if reply.AuthorType != "agent" || reply.AuthorID != agentB {
		t.Fatalf("second author = %s/%s, want agent/%s", reply.AuthorType, reply.AuthorID, agentB)
	}
}

// TestExternalMentionPreviewShowsMailbox pins the composer contract: an
// external target appears as mailbox, never as a native run.
func TestExternalMentionPreviewShowsMailbox(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, _ := externalAgentFixture(t, "Preview External", "daemon-preview")
	issueID := dbfx.Issue(t, "preview external")
	content := fmt.Sprintf("[@Ext](mention://agent/%s) take a look", agentID)
	preview := previewCommentTriggersForTest(t, issueID, map[string]any{"content": content})
	if len(preview.Agents) != 1 {
		t.Fatalf("preview agents = %+v, want 1", preview.Agents)
	}
	if got := preview.Agents[0]; got.ID != agentID || got.DispatchKind != "mailbox" {
		t.Fatalf("preview agent = %+v, want %s/mailbox", got, agentID)
	}
	if len(preview.Blocked) != 0 {
		t.Fatalf("preview blocked = %+v, want none", preview.Blocked)
	}
}

// TestPlainCommentAndReplyDoNotLaunch pins explicit-mention-only routing. A
// plain comment or a reply to an agent-authored comment is discussion; only an
// explicit @ starts work, and a status-only transition never does.
func TestPlainCommentAndReplyDoNotLaunch(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Implicit Suppressed Agent", nil)
	issueID := dbfx.Issue(t, "implicit suppressed", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	taskCount := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID)
	}

	// A plain top-level comment must not start the assignee.
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "just a note"}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("plain comment: %d %s", w.Code, w.Body.String())
	}
	if n := taskCount(); n != 0 {
		t.Fatalf("plain comment created %d tasks, want 0", n)
	}

	// A reply to an agent-authored comment must not wake that agent.
	parentID := dbfx.Comment(t, issueID, "agent result", testutil.Cols{
		"author_type": "agent",
		"author_id":   agentID,
	})
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content":   "thanks, one question",
		"parent_id": parentID,
	}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	if n := taskCount(); n != 0 {
		t.Fatalf("plain reply created %d tasks, want 0", n)
	}

	// A status-only transition must not start a run either.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "in_progress"}), "id", issueID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status update: %d %s", w.Code, w.Body.String())
	}
	if n := taskCount(); n != 0 {
		t.Fatalf("status-only transition created %d tasks, want 0", n)
	}

	// An explicit assignment is still an intentional launch.
	secondIssue := dbfx.Issue(t, "explicit assignment")
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+secondIssue, map[string]any{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	}), "id", secondIssue)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("explicit assign: %d %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, secondIssue); n != 1 {
		t.Fatalf("explicit assign created %d tasks, want 1", n)
	}
}

// TestMailboxDaemonClaimAndFencing covers dedup, daemon/owner binding and
// lease fencing: a stale or foreign ack cannot overwrite a later lease.
func TestMailboxDaemonClaimAndFencing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	recipient, _, ownerID := externalAgentFixture(t, "Mailbox Recipient", "daemon-claim")
	issueID := dbfx.Issue(t, "mailbox fencing")
	commentID := dbfx.Comment(t, issueID, "please review the change")

	create := func() db.AgentMailboxMessage {
		msg, err := testHandler.Queries.CreateAgentMailboxMessage(ctx, db.CreateAgentMailboxMessageParams{
			WorkspaceID:      parseUUID(testWorkspaceID),
			IssueID:          parseUUID(issueID),
			CommentID:        parseUUID(commentID),
			RecipientAgentID: parseUUID(recipient),
			SenderType:       "member",
			SenderID:         parseUUID(testUserID),
		})
		if err != nil {
			t.Fatalf("create mailbox message: %v", err)
		}
		return msg
	}
	first := create()
	// Replaying the same delivery is idempotent.
	if _, err := testHandler.Queries.CreateAgentMailboxMessage(ctx, db.CreateAgentMailboxMessageParams{
		WorkspaceID:      parseUUID(testWorkspaceID),
		IssueID:          parseUUID(issueID),
		CommentID:        parseUUID(commentID),
		RecipientAgentID: parseUUID(recipient),
		SenderType:       "member",
		SenderID:         parseUUID(testUserID),
	}); err == nil {
		t.Fatal("duplicate mailbox insert returned a row; unique dedup did not fire")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1 AND comment_id = $2`, recipient, commentID); n != 1 {
		t.Fatalf("mailbox rows = %d, want 1", n)
	}

	claim := func(daemonID, owner string) (int, []daemonMailboxClaimItem) {
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/daemon/mailbox/claim", map[string]any{"limit": 10})
		c := middleware.WithDaemonContext(r.Context(), testWorkspaceID, daemonID)
		c = middleware.WithDaemonTokenOwnerID(c, owner)
		r = r.WithContext(c)
		testHandler.ClaimDaemonMailbox(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("claim as %s: %d %s", daemonID, w.Code, w.Body.String())
		}
		var resp struct {
			Messages []daemonMailboxClaimItem `json:"messages"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode claim: %v", err)
		}
		return w.Code, resp.Messages
	}
	ack := func(daemonID, owner, messageID, token, status string) int {
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/daemon/mailbox/"+messageID+"/ack", map[string]any{
			"delivery_token": token,
			"status":         status,
		})
		c := middleware.WithDaemonContext(r.Context(), testWorkspaceID, daemonID)
		c = middleware.WithDaemonTokenOwnerID(c, owner)
		r = r.WithContext(c)
		r = withURLParam(r, "messageId", messageID)
		testHandler.AckDaemonMailbox(w, r)
		return w.Code
	}

	// A different (still valid) workspace member cannot claim the owner's
	// sessions: the claim binds session owner as well as daemon.
	if _, notOwner := claim("daemon-claim", testUserID); len(notOwner) != 0 {
		t.Fatalf("another member claimed %d message(s) from this owner's session", len(notOwner))
	}

	_, claimed := claim("daemon-claim", ownerID)
	if len(claimed) != 1 || claimed[0].Message.ID != uuidToString(first.ID) {
		t.Fatalf("claimed = %+v, want one message %s", claimed, uuidToString(first.ID))
	}
	if claimed[0].ExternalSession.SessionID == "" || claimed[0].Content == "" {
		t.Fatalf("claim missing session/content: %+v", claimed[0])
	}
	messageID := claimed[0].Message.ID
	token := claimed[0].DeliveryToken

	// Another daemon cannot ack this lease.
	if code := ack("daemon-other", ownerID, messageID, token, "delivered"); code != http.StatusConflict {
		t.Fatalf("foreign daemon ack status = %d, want 409", code)
	}
	if code := ack("daemon-claim", ownerID, messageID, token, "retry"); code != http.StatusOK {
		t.Fatalf("retry ack status = %d, want 200", code)
	}

	// A retry returns the message to pending with bounded backoff; after the
	// backoff elapses it is claimable again.
	var retryStatus string
	if err := testPool.QueryRow(ctx, `SELECT delivery_status FROM agent_mailbox_message WHERE id = $1`, messageID).Scan(&retryStatus); err != nil {
		t.Fatalf("read retry status: %v", err)
	}
	if retryStatus != "pending" {
		t.Fatalf("retry delivery status = %s, want pending", retryStatus)
	}
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, messageID)
	_, reclaimed := claim("daemon-claim", ownerID)
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed = %+v, want 1", reclaimed)
	}
	newToken := reclaimed[0].DeliveryToken
	if newToken == token {
		t.Fatal("re-claim reused the previous delivery token")
	}
	// Expire the new lease and claim once more; the old token must not ack the
	// newer lease.
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, messageID)
	_, reReclaimed := claim("daemon-claim", ownerID)
	if len(reReclaimed) != 1 {
		t.Fatalf("re-reclaimed = %+v, want 1", reReclaimed)
	}
	if code := ack("daemon-claim", ownerID, messageID, newToken, "delivered"); code != http.StatusConflict {
		t.Fatalf("stale lease ack status = %d, want 409", code)
	}
	if code := ack("daemon-claim", ownerID, messageID, reReclaimed[0].DeliveryToken, "delivered"); code != http.StatusOK {
		t.Fatalf("current lease ack status = %d, want 200", code)
	}
	var deliveryStatus string
	if err := testPool.QueryRow(ctx, `SELECT delivery_status FROM agent_mailbox_message WHERE id = $1`, messageID).Scan(&deliveryStatus); err != nil {
		t.Fatalf("read delivery status: %v", err)
	}
	if deliveryStatus != "delivered" {
		t.Fatalf("delivery status = %s, want delivered", deliveryStatus)
	}
}

func TestMailboxUserCredentialRequiresOwnedDaemonSelector(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	first, _, owner := externalAgentFixture(t, "First host recipient", "daemon-first")
	second, _, _ := externalAgentFixture(t, "Second host recipient", "daemon-second")
	dbfx.Exec(t, `UPDATE agent SET owner_id=$1 WHERE id=$2`, owner, second)
	dbfx.Exec(t, `UPDATE external_agent_session SET owner_user_id=$1 WHERE agent_id=$2`, owner, second)
	dbfx.Runtime(t, "First delivery daemon", testutil.Cols{"daemon_id": "daemon-first", "owner_id": owner})
	dbfx.Runtime(t, "Second delivery daemon", testutil.Cols{"daemon_id": "daemon-second", "owner_id": owner})
	dbfx.Runtime(t, "Foreign delivery daemon", testutil.Cols{"daemon_id": "daemon-foreign"})
	issue := dbfx.Issue(t, "daemon selection")
	for _, recipient := range []string{first, second} {
		comment := dbfx.Comment(t, issue, "request")
		dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": issue, "comment_id": comment,
			"recipient_agent_id": recipient, "sender_type": "member", "sender_id": testUserID,
		})
	}
	claim := func(selector string) *httptest.ResponseRecorder {
		r := newRequest(http.MethodPost, "/api/daemon/mailbox/claim", map[string]any{"limit": 10})
		r.Header.Set("X-User-ID", owner)
		r.Header.Set("X-Multica-Daemon-ID", selector)
		w := httptest.NewRecorder()
		testHandler.ClaimDaemonMailbox(w, r)
		return w
	}
	if w := claim(""); w.Code != http.StatusBadRequest {
		t.Fatalf("missing daemon selector must not claim every host: %d %s", w.Code, w.Body.String())
	}
	if w := claim("daemon-foreign"); w.Code != http.StatusForbidden {
		t.Fatalf("unowned daemon selector: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ daemon, recipient string }{{"daemon-first", first}, {"daemon-second", second}} {
		w := claim(tc.daemon)
		if w.Code != http.StatusOK {
			t.Fatalf("claim %s: %d %s", tc.daemon, w.Code, w.Body.String())
		}
		var result struct {
			Messages []daemonMailboxClaimItem `json:"messages"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Messages) != 1 || result.Messages[0].Message.RecipientAgentID != tc.recipient {
			t.Fatalf("claim crossed daemon binding: %+v", result.Messages)
		}
	}
}

// TestMailboxProcessingAckIsMonotonic pins unread -> read -> handled and the
// rule that read can never undo handled.
func TestMailboxProcessingAckIsMonotonic(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, _, _ := externalAgentFixture(t, "Mailbox Ack", "daemon-ack")
	issueID := dbfx.Issue(t, "mailbox ack")
	commentID := dbfx.Comment(t, issueID, "read me")
	msg, err := testHandler.Queries.CreateAgentMailboxMessage(ctx, db.CreateAgentMailboxMessageParams{
		WorkspaceID:      parseUUID(testWorkspaceID),
		IssueID:          parseUUID(issueID),
		CommentID:        parseUUID(commentID),
		RecipientAgentID: parseUUID(agentID),
		SenderType:       "member",
		SenderID:         parseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("create mailbox message: %v", err)
	}
	ack := func(state string) MailboxMessageResponse {
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/agents/"+agentID+"/mailbox/"+uuidToString(msg.ID)+"/ack", map[string]any{"state": state})
		r = testutil.WithURLParams(r, "id", agentID, "messageId", uuidToString(msg.ID))
		testHandler.AckAgentMailbox(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("ack %s: %d %s", state, w.Code, w.Body.String())
		}
		var resp MailboxMessageResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode ack: %v", err)
		}
		return resp
	}
	if got := ack("read"); got.ProcessingStatus != "read" {
		t.Fatalf("after read = %s, want read", got.ProcessingStatus)
	}
	if got := ack("handled"); got.ProcessingStatus != "handled" {
		t.Fatalf("after handled = %s, want handled", got.ProcessingStatus)
	}
	if got := ack("read"); got.ProcessingStatus != "handled" {
		t.Fatalf("read after handled = %s, want handled (monotonic)", got.ProcessingStatus)
	}
	if got := ack("read"); got.DeliveryStatus != "pending" {
		t.Fatalf("processing ack changed delivery status to %s", got.DeliveryStatus)
	}
}

// TestIssueCoordinatorAndMergePolicy covers native revision/access semantics
// for the coordinator and the human-only merge policy.
func TestIssueCoordinatorAndMergePolicy(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Coordinator Agent", nil)

	// Create derives the coordinator from the dispatch actor.
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "coordinator issue"})
	testHandler.CreateIssue(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create issue: %d %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Coordinator == nil || created.Coordinator.Type != "member" || created.Coordinator.ID != testUserID {
		t.Fatalf("coordinator = %+v, want member/%s", created.Coordinator, testUserID)
	}
	if created.MergeAuthorization.Allowed {
		t.Fatal("merge authorization defaults to allowed; want disallowed")
	}
	if created.ExternalParticipants == nil || created.MailboxDeliveries == nil {
		t.Fatal("external projections must default to empty slices, not null")
	}

	// Explicit coordinator update bumps the revision.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"coordinator": map[string]any{"type": "agent", "id": agentID},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set coordinator: %d %s", w.Code, w.Body.String())
	}
	var updated IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatalf("decode coordinator update: %v", err)
	}
	if updated.Coordinator == nil || updated.Coordinator.Type != "agent" || updated.Coordinator.ID != agentID {
		t.Fatalf("coordinator = %+v, want agent/%s", updated.Coordinator, agentID)
	}
	if updated.Revision <= created.Revision {
		t.Fatalf("revision = %d, want > %d", updated.Revision, created.Revision)
	}

	// A mention does not rewrite the coordinator.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPost, "/api/issues/"+created.ID+"/comments", map[string]any{
		"content": fmt.Sprintf("[@Agent](mention://agent/%s) hello", agentID),
	}), "id", created.ID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("mention comment: %d %s", w.Code, w.Body.String())
	}
	var coordinatorType, coordinatorID string
	if err := testPool.QueryRow(context.Background(), `SELECT coordinator_type, coordinator_id FROM issue WHERE id = $1`, created.ID).Scan(&coordinatorType, &coordinatorID); err != nil {
		t.Fatalf("read coordinator: %v", err)
	}
	if coordinatorType != "agent" || coordinatorID != agentID {
		t.Fatalf("coordinator after mention = %s/%s, want agent/%s", coordinatorType, coordinatorID, agentID)
	}

	// Cross-workspace coordinator references are rejected.
	stranger := dbfx.User(t, "Coordinator Stranger", fmt.Sprintf("coordinator-stranger-%d@test", time.Now().UnixNano()))
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"coordinator": map[string]any{"type": "member", "id": stranger},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-workspace coordinator status = %d, want 400", w.Code)
	}

	// Human grant stores server-set provenance and bumps the revision.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"merge_authorization": map[string]any{
			"allowed":        true,
			"repository_url": "https://github.com/junjzhang/multica",
			"target_branch":  "main",
		},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("grant merge: %d %s", w.Code, w.Body.String())
	}
	var granted IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&granted); err != nil {
		t.Fatalf("decode merge grant: %v", err)
	}
	policy := granted.MergeAuthorization
	if !policy.Allowed || policy.RepositoryURL == nil || *policy.RepositoryURL != "https://github.com/junjzhang/multica" ||
		policy.TargetBranch == nil || *policy.TargetBranch != "main" {
		t.Fatalf("merge policy = %+v, want allowed main", policy)
	}
	if policy.GrantedBy == nil || *policy.GrantedBy != testUserID || policy.GrantedAt == nil {
		t.Fatalf("grant provenance = %+v, want server-set user/time", policy)
	}

	// A machine credential cannot grant or revoke it.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"merge_authorization": map[string]any{"allowed": false},
	}), "id", created.ID)
	r.Header.Set("X-Actor-Source", "external_agent")
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("machine merge revoke status = %d, want 403", w.Code)
	}

	// A human revoke clears the stored coordinates.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"merge_authorization": map[string]any{"allowed": false},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke merge: %d %s", w.Code, w.Body.String())
	}
	var revoked IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&revoked); err != nil {
		t.Fatalf("decode merge revoke: %v", err)
	}
	if revoked.MergeAuthorization.Allowed || revoked.MergeAuthorization.RepositoryURL != nil || revoked.MergeAuthorization.TargetBranch != nil {
		t.Fatalf("revoked merge policy = %+v, want disallowed and cleared", revoked.MergeAuthorization)
	}
}

// TestExternalAgentRegistrationAndTokens covers the HTTP registration shape,
// idempotent re-registration, and raw-token-once semantics.
func TestExternalAgentRegistrationAndTokens(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	daemonID := fmt.Sprintf("daemon-register-%d", time.Now().UnixNano())
	dbfx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"daemon_id":    daemonID,
		"name":         "Registration Daemon",
		"runtime_mode": "cloud",
		"provider":     "paseo",
		"status":       "online",
		"device_info":  "",
		"metadata":     testutil.Raw("'{}'::jsonb"),
		"last_seen_at": testutil.Raw("now()"),
		"visibility":   "private",
		"owner_id":     testUserID,
	})
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	body := map[string]any{
		"name": "External Registration " + suffix,
		"external_session": map[string]any{
			"provider":     "paseo",
			"host_id":      "host-" + suffix,
			"session_id":   "session-" + suffix,
			"display_name": "Registered Session",
			"url":          "paseo://h/host-" + suffix + "/agent/session-" + suffix,
			"daemon_id":    daemonID,
		},
	}
	w := httptest.NewRecorder()
	testHandler.CreateExternalAgent(w, newRequest(http.MethodPost, "/api/agents/external", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("register external agent: %d %s", w.Code, w.Body.String())
	}
	var created AgentResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if created.ExecutionKind != "external" || created.RuntimeBound || created.RuntimeID != "" {
		t.Fatalf("registered agent = kind %s runtime_bound %v runtime %q, want external/unbound/empty", created.ExecutionKind, created.RuntimeBound, created.RuntimeID)
	}
	if created.ExternalSession == nil || created.ExternalSession.DaemonID != daemonID || created.ExternalSession.SessionID != "session-"+suffix {
		t.Fatalf("external_session = %+v", created.ExternalSession)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM external_agent_token WHERE agent_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM external_agent_session WHERE agent_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, created.ID)
	})

	// Re-registering the same session returns the same identity.
	w = httptest.NewRecorder()
	testHandler.CreateExternalAgent(w, newRequest(http.MethodPost, "/api/agents/external", body))
	if w.Code != http.StatusOK {
		t.Fatalf("re-register: %d %s", w.Code, w.Body.String())
	}
	var again AgentResponse
	if err := json.NewDecoder(w.Body).Decode(&again); err != nil {
		t.Fatalf("decode re-registration: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("re-registration created %s, want %s", again.ID, created.ID)
	}

	// Invalid link shapes are rejected.
	badBody := map[string]any{
		"name": "Bad Link " + suffix,
		"external_session": map[string]any{
			"provider":   "paseo",
			"host_id":    "host-bad-" + suffix,
			"session_id": "session-bad-" + suffix,
			"url":        "paseo://user:pass@h/server/agent/s",
			"daemon_id":  daemonID,
		},
	}
	w = httptest.NewRecorder()
	testHandler.CreateExternalAgent(w, newRequest(http.MethodPost, "/api/agents/external", badBody))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("credential-bearing link status = %d, want 400", w.Code)
	}

	// Machine credentials cannot register identities.
	w = httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/agents/external", body)
	r.Header.Set("X-Actor-Source", "task_token")
	testHandler.CreateExternalAgent(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("machine registration status = %d, want 403", w.Code)
	}

	// Token mint returns the raw value once, stores only a hash, and revoke
	// makes it immediately unusable.
	expires := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	w = httptest.NewRecorder()
	r = newRequest(http.MethodPost, "/api/agents/"+created.ID+"/tokens", map[string]any{"expires_at": expires})
	r = withURLParam(r, "id", created.ID)
	testHandler.CreateExternalAgentToken(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint token: %d %s", w.Code, w.Body.String())
	}
	var minted createExternalAgentTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&minted); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if !strings.HasPrefix(minted.Token, auth.ExternalAgentTokenPrefix) {
		t.Fatalf("token prefix = %q, want mea_", minted.Token)
	}
	var storedHash string
	if err := testPool.QueryRow(context.Background(), `SELECT token_hash FROM external_agent_token WHERE id = $1`, minted.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if storedHash == minted.Token || storedHash != auth.HashToken(minted.Token) {
		t.Fatal("stored credential is not the token hash")
	}
	if w, _ := serveExternalAuth(t, minted.Token, nil); w.Code != http.StatusOK {
		t.Fatalf("fresh token auth status = %d, want 200", w.Code)
	}
	w = httptest.NewRecorder()
	r = testutil.WithURLParams(newRequest(http.MethodDelete, "/api/agents/"+created.ID+"/tokens/"+minted.ID, nil), "id", created.ID, "tokenId", minted.ID)
	testHandler.RevokeExternalAgentToken(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke token: %d %s", w.Code, w.Body.String())
	}
	if w, _ := serveExternalAuth(t, minted.Token, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token auth status = %d, want 401", w.Code)
	}
}

// TestExternalAgentTokenCannotUseDaemonEndpoints pins the credential's scope:
// a mailbox token is not a machine credential for daemon claim/runtime APIs.
func TestExternalAgentTokenCannotUseDaemonEndpoints(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, ownerID := externalAgentFixture(t, "Daemon Boundary", "daemon-boundary")
	raw := externalTokenFixture(t, agentID, ownerID, time.Now().Add(time.Hour))

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("daemon route must not run for an external agent token")
	})
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/mailbox/claim", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	middleware.DaemonAuth(testHandler.Queries, nil, nil, nil)(next).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("daemon endpoint with mea_ token = %d, want 403: %s", w.Code, w.Body.String())
	}
}
