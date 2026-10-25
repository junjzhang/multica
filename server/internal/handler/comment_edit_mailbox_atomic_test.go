package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// editCommentRequest drives UpdateComment and decodes the response only on
// success; a failed edit has no comment body to report.
func editCommentRequest(t *testing.T, commentID string, body map[string]any) (int, CommentResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID, body), "commentId", commentID)
	testHandler.UpdateComment(w, r)
	var resp CommentResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode comment response: %v", err)
		}
	}
	return w.Code, resp
}

// clearMailboxInsertFailures drops the injection triggers a test installed so
// the same edit can be retried against a healthy mailbox.
func clearMailboxInsertFailures(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `DO $$ DECLARE r record; BEGIN FOR r IN SELECT tgname FROM pg_trigger WHERE tgrelid='agent_mailbox_message'::regclass AND tgname LIKE 'test_mailbox_fail_%' LOOP EXECUTE 'DROP TRIGGER '||quote_ident(r.tgname)||' ON agent_mailbox_message'; END LOOP; END $$`)
}

func storedCommentContent(t *testing.T, commentID string) string {
	t.Helper()
	var content string
	if err := testPool.QueryRow(t.Context(), `SELECT content FROM comment WHERE id = $1`, commentID).Scan(&content); err != nil {
		t.Fatalf("read comment content: %v", err)
	}
	return content
}

func postPlainComment(t *testing.T, issueID string) CommentResponse {
	t.Helper()
	w, resp := postMentionComment(t, issueID)
	if w.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", w.Code, w.Body.String())
	}
	return resp
}

// TestEditCommentAddedMentionPersistsDeliveryAtomically is the strict-edit
// contract: content and every valid external handoff are one database
// outcome. An insert failure returns an error, leaves the old content in
// place, stores no delivery, and the same edit succeeds on retry.
func TestEditCommentAddedMentionPersistsDeliveryAtomically(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, _ := externalAgentFixture(t, "Edit Strict Target", "daemon-edit-strict")
	issueID := dbfx.Issue(t, "edit strict mention")
	created := postPlainComment(t, issueID)
	original := storedCommentContent(t, created.ID)
	content := fmt.Sprintf("[@Target](mention://agent/%s) please review", agentID)
	body := map[string]any{"content": content, "expected_revision": created.Revision}

	installMailboxInsertFailure(t, "")
	code, _ := editCommentRequest(t, created.ID, body)
	if code != http.StatusInternalServerError {
		t.Fatalf("failed edit status = %d, want 500: a mailbox failure must fail the edit", code)
	}
	if got := storedCommentContent(t, created.ID); got != original {
		t.Fatalf("content after failed edit = %q, want original %q", got, original)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, agentID); n != 0 {
		t.Fatalf("mailbox rows after failed edit = %d, want 0", n)
	}

	clearMailboxInsertFailures(t)
	code, resp := editCommentRequest(t, created.ID, body)
	if code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if got := storedCommentContent(t, created.ID); got != content {
		t.Fatalf("content after retry = %q, want %q", got, content)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1 AND comment_id = $2`, agentID, created.ID); n != 1 {
		t.Fatalf("mailbox rows after retry = %d, want 1", n)
	}
	outcome := findCommentOutcome(t, resp.TriggerOutcomes, agentID)
	if outcome.Status != DispatchQueued || outcome.DispatchKind != "mailbox" {
		t.Fatalf("outcome = %+v, want queued/mailbox", outcome)
	}
}

// TestEditCommentAddedMentionNonStrictRollsBack covers the legacy update shape
// (no content_base / expected_revision): the same transaction boundary must
// hold even though cancellation happens before the write.
func TestEditCommentAddedMentionNonStrictRollsBack(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, _ := externalAgentFixture(t, "Edit Legacy Target", "daemon-edit-legacy")
	issueID := dbfx.Issue(t, "edit legacy mention")
	created := postPlainComment(t, issueID)
	original := storedCommentContent(t, created.ID)
	content := fmt.Sprintf("[@Target](mention://agent/%s) please review", agentID)
	body := map[string]any{"content": content}

	installMailboxInsertFailure(t, "")
	code, _ := editCommentRequest(t, created.ID, body)
	if code != http.StatusInternalServerError {
		t.Fatalf("failed edit status = %d, want 500", code)
	}
	if got := storedCommentContent(t, created.ID); got != original {
		t.Fatalf("content after failed edit = %q, want original %q", got, original)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, agentID); n != 0 {
		t.Fatalf("mailbox rows after failed edit = %d, want 0", n)
	}

	clearMailboxInsertFailures(t)
	code, resp := editCommentRequest(t, created.ID, body)
	if code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1 AND comment_id = $2`, agentID, created.ID); n != 1 {
		t.Fatalf("mailbox rows after retry = %d, want 1", n)
	}
	outcome := findCommentOutcome(t, resp.TriggerOutcomes, agentID)
	if outcome.Status != DispatchQueued || outcome.DispatchKind != "mailbox" {
		t.Fatalf("outcome = %+v, want queued/mailbox", outcome)
	}
}

// TestEditCommentMailboxFailureIsAllOrNothingForMultipleRecipients fails the
// second recipient and requires the first recipient's insert and the new
// content to roll back with it.
func TestEditCommentMailboxFailureIsAllOrNothingForMultipleRecipients(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	firstAgent, _, _ := externalAgentFixture(t, "Edit Multi First", "daemon-edit-multi")
	secondAgent, _, _ := externalAgentFixture(t, "Edit Multi Second", "daemon-edit-multi")
	issueID := dbfx.Issue(t, "edit multi recipient")
	created := postPlainComment(t, issueID)
	original := storedCommentContent(t, created.ID)
	content := fmt.Sprintf("[@First](mention://agent/%s) and [@Second](mention://agent/%s) please review", firstAgent, secondAgent)
	body := map[string]any{"content": content, "expected_revision": created.Revision}

	installMailboxInsertFailure(t, secondAgent)
	code, _ := editCommentRequest(t, created.ID, body)
	if code != http.StatusInternalServerError {
		t.Fatalf("failed edit status = %d, want 500", code)
	}
	if got := storedCommentContent(t, created.ID); got != original {
		t.Fatalf("content after failed edit = %q, want original %q", got, original)
	}
	if n := countRows(t,
		`SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id IN ($1, $2)`,
		firstAgent, secondAgent); n != 0 {
		t.Fatalf("mailbox rows after failed edit = %d, want 0: partial recipient persistence is not allowed", n)
	}

	clearMailboxInsertFailures(t)
	code, _ = editCommentRequest(t, created.ID, body)
	if code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if n := countRows(t,
		`SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id IN ($1, $2) AND comment_id = $3`,
		firstAgent, secondAgent, created.ID); n != 2 {
		t.Fatalf("mailbox rows after retry = %d, want 2", n)
	}
}

// TestEditCommentMailboxRespectsSuppressionAndPermission pins the two paths
// that store nothing: a suppressed target is skipped before enqueue, and a
// target the editor may not invoke is reported blocked without a delivery.
func TestEditCommentMailboxRespectsSuppressionAndPermission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	allowedAgent, _, _ := externalAgentFixture(t, "Edit Allowed Target", "daemon-edit-policy")
	suppressedAgent, _, _ := externalAgentFixture(t, "Edit Suppressed Target", "daemon-edit-policy")
	issueID := dbfx.Issue(t, "edit mailbox policy")
	created := postPlainComment(t, issueID)
	content := fmt.Sprintf("[@Allowed](mention://agent/%s) and [@Suppressed](mention://agent/%s) please review", allowedAgent, suppressedAgent)

	code, resp := editCommentRequest(t, created.ID, map[string]any{
		"content":            content,
		"expected_revision":  created.Revision,
		"suppress_agent_ids": []string{suppressedAgent},
	})
	if code != http.StatusOK {
		t.Fatalf("suppressed edit status = %d, want 200", code)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, allowedAgent); n != 1 {
		t.Fatalf("allowed rows = %d, want 1", n)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, suppressedAgent); n != 0 {
		t.Fatalf("suppressed rows = %d, want 0", n)
	}
	for _, o := range resp.TriggerOutcomes {
		if o.TargetID == suppressedAgent {
			t.Fatalf("suppressed target produced an outcome: %+v", o)
		}
	}

	// A private agent owned by someone else cannot be invoked by this editor.
	otherOwner := dbfx.User(t, "Edit Private Owner", fmt.Sprintf("edit-private-%d@external.test", time.Now().UnixNano()))
	privateAgent := dbfx.Agent(t, "Edit Private Target", "", testutil.Cols{
		"owner_id":        otherOwner,
		"execution_kind":  "external",
		"visibility":      "private",
		"permission_mode": "private",
	})
	blockedContent := content + fmt.Sprintf(" [@Private](mention://agent/%s)", privateAgent)
	code, resp = editCommentRequest(t, created.ID, map[string]any{
		"content":           blockedContent,
		"expected_revision": resp.Revision,
	})
	if code != http.StatusOK {
		t.Fatalf("blocked mention edit status = %d, want 200", code)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, privateAgent); n != 0 {
		t.Fatalf("blocked target rows = %d, want 0", n)
	}
	outcome := findCommentOutcome(t, resp.TriggerOutcomes, privateAgent)
	if outcome.Status != DispatchBlocked || outcome.ReasonCode != ReasonInvocationNotAllowed {
		t.Fatalf("blocked outcome = %+v, want blocked/%s", outcome, ReasonInvocationNotAllowed)
	}
}
