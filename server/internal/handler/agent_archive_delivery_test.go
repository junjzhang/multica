package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Archiving an agent withdraws its unaccepted notifications in the transaction
// that writes the archive. A pending notification must never stay claimable for
// an agent that no longer exists.
func TestArchiveAgentWithdrawsUnacceptedNotification(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	agentID, sessionID, _ := externalAgentFixture(t, "Archive Mailbox", "daemon-archive-"+suffix)
	deliveryID := dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
		"workspace_id":       testWorkspaceID,
		"recipient_agent_id": agentID,
		"recipient_type":     "agent",
		"target_session_id":  sessionID,
		"source_type":        "ci_run",
		"source_key":         "archive:" + suffix,
		"kind":               "ci_terminal",
		"payload":            testutil.Raw(`'{"run_id":"archive"}'::jsonb`),
	})

	w := httptest.NewRecorder()
	testHandler.ArchiveAgent(w, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agentID+"/archive", nil), "id", agentID))
	if w.Code != http.StatusOK {
		t.Fatalf("ArchiveAgent: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var status string
	var withdrawnAt pgtype.Timestamptz
	if err := testPool.QueryRow(context.Background(),
		`SELECT delivery_status, withdrawn_at FROM agent_mailbox_message WHERE id = $1`, deliveryID).
		Scan(&status, &withdrawnAt); err != nil {
		t.Fatalf("read archived notification: %v", err)
	}
	if status != "failed" {
		t.Fatalf("notification of an archived agent = %q, want failed", status)
	}
	if !withdrawnAt.Valid {
		t.Fatal("notification of an archived agent carries no withdrawal marker")
	}
}
