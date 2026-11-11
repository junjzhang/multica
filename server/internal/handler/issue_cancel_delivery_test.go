package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// A client that PATCHes the whole issue back (including its current terminal
// status) is not cancelling it again. Such a write must not withdraw a
// notification that was created after the cancellation.
func TestRepeatedClosedStatusWriteKeepsPostCancelMention(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	issueID := dbfx.Issue(t, "Repeated close status "+suffix)
	agentID, sessionID, _ := externalAgentFixture(t, "Repeated Close", "daemon-repeat-"+suffix)
	bindExternalSession(t, agentID, sessionID, issueID)

	writeStatus := func() {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.UpdateIssue(w, withURLParam(
			newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "cancelled"}), "id", issueID))
		if w.Code != http.StatusOK {
			t.Fatalf("UpdateIssue cancelled: %d %s", w.Code, w.Body.String())
		}
	}

	writeStatus()

	mention := fmt.Sprintf("[@Receiver](mention://agent/%s) new request after cancellation", agentID)
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": mention}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: %d %s", w.Code, w.Body.String())
	}
	var comment CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&comment); err != nil {
		t.Fatalf("decode comment: %v", err)
	}

	state := func() (string, pgtype.Timestamptz) {
		t.Helper()
		var status string
		var withdrawnAt pgtype.Timestamptz
		if err := testPool.QueryRow(context.Background(),
			`SELECT delivery_status, withdrawn_at FROM agent_mailbox_message WHERE comment_id = $1`,
			parseUUID(comment.ID)).Scan(&status, &withdrawnAt); err != nil {
			t.Fatalf("read post-cancel notification: %v", err)
		}
		return status, withdrawnAt
	}

	if status, _ := state(); status != "pending" {
		t.Fatalf("post-cancel notification = %q, want pending", status)
	}
	// The full-object write resends the status the issue already has.
	writeStatus()
	status, withdrawnAt := state()
	if status != "pending" || withdrawnAt.Valid {
		t.Fatalf("repeated closed-status write changed the post-cancel notification to %q (withdrawn %+v)", status, withdrawnAt)
	}
}
