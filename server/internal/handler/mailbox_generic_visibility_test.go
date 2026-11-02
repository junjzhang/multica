package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/middleware"
)

// TestGenericDeliveryHiddenFromLegacyMailboxReaders pins the compatibility
// boundary of the shared storage. The frozen mailbox DTO and the daemon claim
// render an issue/comment prompt; a generic (non-comment) delivery has neither,
// so both legacy readers must hide it even though its row and GetDelivery are
// live. Listing and claiming a real comment delivery in the same test proves
// the readers still work rather than returning nothing at all.
func TestGenericDeliveryHiddenFromLegacyMailboxReaders(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, ownerID := externalAgentFixture(t, "Generic Visibility Target", "daemon-generic")
	issueID := dbfx.Issue(t, "generic visibility")
	bindExternalSession(t, agentID, sessionID, issueID)
	if w, _ := postMentionComment(t, issueID, agentID); w.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", w.Code, w.Body.String())
	}
	var commentDeliveryID string
	if err := testPool.QueryRow(t.Context(),
		`SELECT id FROM agent_mailbox_message WHERE recipient_agent_id = $1 AND source_type = 'comment'`,
		agentID).Scan(&commentDeliveryID); err != nil {
		t.Fatalf("read comment delivery: %v", err)
	}

	store := delivery.NewPostgres(testHandler.Queries)
	generic, err := store.Enqueue(t.Context(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: testWorkspaceID},
		Source:          delivery.Source{Type: "ci", Key: "generic-visibility-run-1"},
		Target:          delivery.Target{SessionID: sessionID},
		Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
		NotificationKey: "ci:generic-visibility-run-1",
		Kind:            "ci_terminal",
		Payload:         json.RawMessage(`{"run":1}`),
	})
	if err != nil {
		t.Fatalf("enqueue generic: %v", err)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_mailbox_message WHERE id = $1`, generic.ID)
	if _, err := store.Get(t.Context(), generic.Scope, generic.ID); err != nil {
		t.Fatalf("generic delivery is not readable through the core: %v", err)
	}

	// The frozen list projection excludes the generic row.
	lw := httptest.NewRecorder()
	lr := withURLParam(newRequest(http.MethodGet, "/api/agents/"+agentID+"/mailbox", nil), "id", agentID)
	testHandler.ListAgentMailbox(lw, lr)
	if lw.Code != http.StatusOK {
		t.Fatalf("list: %d %s", lw.Code, lw.Body.String())
	}
	var listed struct {
		Messages []MailboxMessageResponse `json:"messages"`
	}
	if err := json.NewDecoder(lw.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Messages) != 1 || listed.Messages[0].ID != commentDeliveryID {
		t.Fatalf("listed messages = %+v, want only comment delivery %s", listed.Messages, commentDeliveryID)
	}

	// The daemon claim leases only the comment row; the generic row is neither
	// returned nor touched.
	cw := httptest.NewRecorder()
	cr := newRequest(http.MethodPost, "/api/daemon/mailbox/claim", map[string]any{"limit": 10})
	ctx := middleware.WithDaemonContext(cr.Context(), testWorkspaceID, "daemon-generic")
	ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
	testHandler.ClaimDaemonMailbox(cw, cr.WithContext(ctx))
	if cw.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", cw.Code, cw.Body.String())
	}
	var claim struct {
		Messages []daemonMailboxClaimItem `json:"messages"`
	}
	if err := json.NewDecoder(cw.Body).Decode(&claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if len(claim.Messages) != 1 || claim.Messages[0].Message.ID != commentDeliveryID {
		t.Fatalf("claimed messages = %+v, want only comment delivery %s", claim.Messages, commentDeliveryID)
	}
	if n := countRows(t, `SELECT attempts FROM agent_mailbox_message WHERE id = $1`, generic.ID); n != 0 {
		t.Fatalf("generic row attempts = %d, want 0: the claim must not touch it", n)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE id = $1 AND delivery_status = 'pending'`, generic.ID); n != 1 {
		t.Fatalf("generic delivery status changed; want still pending")
	}
}
