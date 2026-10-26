package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// installMailboxInsertFailure makes agent_mailbox_message inserts fail for one
// recipient (or for every recipient when failForRecipient is empty). The
// trigger is the storage-level injection point: it proves atomicity without
// replacing the delivery core with a test double.
func installMailboxInsertFailure(t *testing.T, failForRecipient string) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	function := "test_mailbox_fail_" + suffix
	trigger := "test_mailbox_fail_" + suffix
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF TG_ARGV[0] = '' OR NEW.recipient_agent_id = TG_ARGV[0]::uuid THEN
				RAISE EXCEPTION 'injected mailbox insert failure';
			END IF;
			RETURN NEW;
		END
		$$`, function))
	// Cleanup runs LIFO: the trigger must be dropped before its function.
	dbfx.Cleanup(t, "DROP FUNCTION IF EXISTS "+function+"()")
	dbfx.Cleanup(t, "DROP TRIGGER IF EXISTS "+trigger+" ON agent_mailbox_message")
	dbfx.Exec(t, fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE INSERT ON agent_mailbox_message FOR EACH ROW EXECUTE FUNCTION %s('%s')`,
		trigger, function, failForRecipient,
	))
}

func postMentionComment(t *testing.T, issueID string, agentIDs ...string) (*httptest.ResponseRecorder, CommentResponse) {
	t.Helper()
	mentions := make([]string, 0, len(agentIDs))
	for i, id := range agentIDs {
		mentions = append(mentions, fmt.Sprintf("[@Agent%d](mention://agent/%s)", i, id))
	}
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": strings.Join(mentions, " and ") + " please review",
	})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	var resp CommentResponse
	if w.Code == http.StatusCreated {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode comment response: %v", err)
		}
	}
	return w, resp
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

// TestCommentAndMailboxRollBackTogether injects a mailbox insert failure and
// requires the comment to be absent as well: the saved comment and its delivery
// intent are one database outcome.
func TestCommentAndMailboxRollBackTogether(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, _ := externalAgentFixture(t, "Atomic Mailbox Recipient", "daemon-atomic")
	issueID := dbfx.Issue(t, "atomic mailbox comment")
	installMailboxInsertFailure(t, "")

	w, _ := postMentionComment(t, issueID, agentID)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 after mailbox failure: %s", w.Code, w.Body.String())
	}
	if count := countRows(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND deleted_at IS NULL`, issueID); count != 0 {
		t.Fatalf("comments = %d, want 0: the comment committed without its handoff", count)
	}
	if count := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, agentID); count != 0 {
		t.Fatalf("mailbox rows = %d, want 0", count)
	}
}

// TestCommentMailboxFailureIsAllOrNothingForMultipleRecipients fails the second
// recipient's insert and asserts the first recipient's row rolled back too.
// The boundary is the comment transaction, not a partial per-recipient commit.
func TestCommentMailboxFailureIsAllOrNothingForMultipleRecipients(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	firstAgent, _, _ := externalAgentFixture(t, "Atomic Mailbox First", "daemon-atomic")
	secondAgent, _, _ := externalAgentFixture(t, "Atomic Mailbox Second", "daemon-atomic")
	issueID := dbfx.Issue(t, "atomic multi-recipient mailbox")
	installMailboxInsertFailure(t, secondAgent)

	w, _ := postMentionComment(t, issueID, firstAgent, secondAgent)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 after second recipient failure: %s", w.Code, w.Body.String())
	}
	if count := countRows(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND deleted_at IS NULL`, issueID); count != 0 {
		t.Fatalf("comments = %d, want 0", count)
	}
	if count := countRows(t,
		`SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id IN ($1, $2)`,
		firstAgent, secondAgent); count != 0 {
		t.Fatalf("mailbox rows = %d, want 0: partial recipient persistence is not allowed", count)
	}
}

// TestQuickActionMailboxDeliveryRollsBackComment covers the second comment
// producer: a quick action renders an ordinary comment carrying the target
// mention, so it must use the same atomic handoff boundary.
func TestQuickActionMailboxDeliveryRollsBackComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, _ := externalAgentFixture(t, "Atomic Quick Action Target", "daemon-atomic")
	var action QuickActionResponse
	testutil.Call(t, testHandler.CreateQuickAction, newRequest(http.MethodPost, "/api/quick-actions", map[string]any{
		"name": "Atomic quick action", "prompt": "review the change", "visibility": "public",
		"assignee_type": "agent", "assignee_id": agentID,
	})).Want(http.StatusCreated).JSON(&action)
	dbfx.Cleanup(t, `DELETE FROM quick_action WHERE id = $1`, action.ID)

	issueID := dbfx.Issue(t, "atomic quick action issue")
	installMailboxInsertFailure(t, "")

	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/quick-actions/"+action.ID, nil)
	r = testutil.WithURLParams(r, "id", issueID, "quickActionId", action.ID)
	testHandler.RunQuickAction(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 after mailbox failure: %s", w.Code, w.Body.String())
	}
	if count := countRows(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND deleted_at IS NULL`, issueID); count != 0 {
		t.Fatalf("comments = %d, want 0", count)
	}
	if count := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id = $1`, agentID); count != 0 {
		t.Fatalf("mailbox rows = %d, want 0", count)
	}
}

// reaches the delivery core rather than the legacy insert: the idempotency key,
// kind and immutable payload are populated on the committed row.
func TestCommentMailboxDeliveryWritesCoreFields(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, _ := externalAgentFixture(t, "Atomic Mailbox Fields", "daemon-atomic")
	issueID := dbfx.Issue(t, "atomic mailbox fields")

	w, resp := postMentionComment(t, issueID, agentID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if resp.ID == "" {
		t.Fatal("comment response has no id")
	}

	var sourceType, sourceKey, kind, senderType string
	var payload []byte
	if err := testPool.QueryRow(t.Context(), `
		SELECT source_type, source_key, kind, sender_type, payload
		FROM agent_mailbox_message
		WHERE recipient_agent_id = $1 AND comment_id = $2
	`, agentID, resp.ID).Scan(&sourceType, &sourceKey, &kind, &senderType, &payload); err != nil {
		t.Fatalf("read mailbox row: %v", err)
	}
	if sourceType != "comment" || sourceKey != resp.ID || kind != "comment_mention" || senderType != "member" {
		t.Fatalf("row = source_type %q source_key %q kind %q sender_type %q", sourceType, sourceKey, kind, senderType)
	}
	var decoded struct {
		Schema    string `json:"schema"`
		IssueID   string `json:"issue_id"`
		CommentID string `json:"comment_id"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.Schema != "comment_mention.v1" || decoded.IssueID != issueID || decoded.CommentID != resp.ID {
		t.Fatalf("payload = %+v", decoded)
	}
}
