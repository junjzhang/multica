package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// reopenBeforeWriteTxStarter runs a callback just before the first write
// transaction starts. It gives the test a second writer (reopen plus a new
// mention) between the request's unlocked issue read and its status write.
type reopenBeforeWriteTxStarter struct {
	inner txStarter
	fired bool
	hook  func()
}

func (h *reopenBeforeWriteTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	if !h.fired {
		h.fired = true
		h.hook()
	}
	return h.inner.Begin(ctx)
}

// A PATCH that read the issue as cancelled can still land on a row another
// writer reopened to todo. The close rule must use the status under the write
// transaction's row lock, not the earlier request snapshot, or this real
// todo->cancelled transition leaves the open-period notification deliverable.
func TestClosedStatusWriteUsesLockedPreviousStatus(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	issueID := dbfx.Issue(t, "Locked previous status "+suffix)
	agentID, sessionID, _ := externalAgentFixture(t, "Locked Previous", "daemon-locked-prev-"+suffix)
	bindExternalSession(t, agentID, sessionID, issueID)

	cancel := func() {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.UpdateIssue(w, withURLParam(
			newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "cancelled"}), "id", issueID))
		if w.Code != http.StatusOK {
			t.Fatalf("UpdateIssue cancelled: %d %s", w.Code, w.Body.String())
		}
	}
	cancel()

	var commentID string
	originalStarter := testHandler.TxStarter
	hooked := &reopenBeforeWriteTxStarter{inner: originalStarter}
	hooked.hook = func() {
		reopen := httptest.NewRecorder()
		testHandler.UpdateIssue(reopen, withURLParam(
			newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "todo"}), "id", issueID))
		if reopen.Code != http.StatusOK {
			t.Errorf("reopen: %d %s", reopen.Code, reopen.Body.String())
			return
		}
		mention := fmt.Sprintf("[@Receiver](mention://agent/%s) while open", agentID)
		w := httptest.NewRecorder()
		testHandler.CreateComment(w, withURLParam(
			newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": mention}), "id", issueID))
		if w.Code != http.StatusCreated {
			t.Errorf("CreateComment: %d %s", w.Code, w.Body.String())
			return
		}
		var comment CommentResponse
		if err := json.NewDecoder(w.Body).Decode(&comment); err != nil {
			t.Errorf("decode comment: %v", err)
			return
		}
		commentID = comment.ID
	}
	testHandler.TxStarter = hooked
	defer func() { testHandler.TxStarter = originalStarter }()

	// The request loaded the issue as cancelled before the hook reopened it.
	cancel()

	var issueStatus, delivery string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM issue WHERE id = $1`, parseUUID(issueID)).Scan(&issueStatus); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if commentID == "" {
		t.Fatal("the concurrent writer did not create the open-period mention")
	}
	if err := testPool.QueryRow(context.Background(),
		`SELECT delivery_status FROM agent_mailbox_message WHERE comment_id = $1`,
		parseUUID(commentID)).Scan(&delivery); err != nil {
		t.Fatalf("read open-period notification: %v", err)
	}
	if issueStatus != "cancelled" {
		t.Fatalf("issue status = %q, want cancelled", issueStatus)
	}
	if delivery == "pending" || delivery == "leased" {
		t.Fatalf("issue is cancelled (todo->cancelled transition) but its open-period notification stays %q", delivery)
	}
}
