package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestGenericDeliveryStaysOutOfLegacyMailboxList pins the compatibility
// boundary of the shared storage. The frozen user-side mailbox DTO renders an
// issue/comment notification and a generic (non-comment) delivery has neither,
// so the list must hide it even though the delivery worker claims it through
// the same path. Listing a real comment delivery proves the reader still works,
// and claiming both proves there is only one delivery consumer.
func TestGenericDeliveryStaysOutOfLegacyMailboxList(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, ownerID := externalAgentFixture(t, "Generic Visibility Target", "daemon-generic")
	genericSessionID := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "generic-visibility-host-" + agentID,
		"session_id":    "generic-visibility-session-" + agentID,
		"display_name":  "Generic Visibility Second Session",
		"daemon_id":     "daemon-generic",
	})
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
		Target:          delivery.Target{SessionID: genericSessionID},
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

	// The one delivery claim leases both rows: the comment notification no
	// longer has a consumer of its own, and the generic row is no longer hidden
	// from delivery.
	cw := httptest.NewRecorder()
	cr := newRequest(http.MethodPost, "/api/daemon/deliveries/claim", map[string]any{"limit": 10})
	ctx := middleware.WithDaemonContext(cr.Context(), testWorkspaceID, "daemon-generic")
	ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
	testHandler.ClaimDaemonDeliveries(cw, cr.WithContext(ctx))
	if cw.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", cw.Code, cw.Body.String())
	}
	var claim struct {
		Deliveries []daemonDeliveryClaim `json:"deliveries"`
	}
	if err := json.NewDecoder(cw.Body).Decode(&claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	claimed := map[string]daemonDeliveryClaim{}
	for _, item := range claim.Deliveries {
		claimed[item.ID] = item
	}
	if _, ok := claimed[commentDeliveryID]; !ok {
		t.Fatalf("claimed deliveries = %+v, want comment delivery %s", claim.Deliveries, commentDeliveryID)
	}
	genericClaim, ok := claimed[generic.ID]
	if !ok {
		t.Fatalf("claimed deliveries = %+v, want generic delivery %s", claim.Deliveries, generic.ID)
	}
	if genericClaim.SourceType != "ci" || genericClaim.IssueID != "" {
		t.Fatalf("generic claim = %+v, want its own source and no issue ref", genericClaim)
	}
	if n := countRows(t, `SELECT attempts FROM agent_mailbox_message WHERE id = $1`, generic.ID); n != 1 {
		t.Fatalf("generic row attempts = %d, want 1: the unified claim must lease it", n)
	}
}
