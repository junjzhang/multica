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

// TestDeliveryClaimCoversCommentAndGeneric pins the one consumer contract:
// comment notifications and generic events are leased by the same claim, each
// carrying its stable source references, and an expired lease for either is
// verification-only instead of deliverable again.
func TestDeliveryClaimCoversCommentAndGeneric(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, ownerID := externalAgentFixture(t, "Unified Delivery Target", "daemon-unified-worker")
	secondSessionID := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "unified-second-host-" + agentID,
		"session_id":    "unified-second-session-" + agentID,
		"display_name":  "Unified Second Session",
		"daemon_id":     "daemon-unified-worker",
	})
	issueID := dbfx.Issue(t, "unified delivery claim")
	commentID := dbfx.Comment(t, issueID, "please pick this up")
	store := delivery.NewPostgres(testHandler.Queries)
	comment, err := store.Enqueue(t.Context(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: testWorkspaceID},
		Source:          delivery.Source{Type: "comment", Key: commentID},
		Target:          delivery.Target{SessionID: sessionID},
		Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
		NotificationKey: "comment:" + commentID + ":agent:" + agentID,
		Kind:            "comment_mention",
		Payload:         json.RawMessage(`{"schema":"comment_mention.v1","issue_id":"` + issueID + `","comment_id":"` + commentID + `"}`),
		Refs:            delivery.Refs{IssueID: issueID, CommentID: commentID},
	})
	if err != nil {
		t.Fatalf("enqueue comment: %v", err)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_mailbox_message WHERE id = $1`, comment.ID)
	generic, err := store.Enqueue(t.Context(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: testWorkspaceID},
		Source:          delivery.Source{Type: "ci_run", Key: "generic-claim-run-1"},
		Target:          delivery.Target{SessionID: secondSessionID},
		Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
		NotificationKey: "ci:generic-claim-run-1",
		Kind:            "ci_terminal",
		Payload:         json.RawMessage(`{"run_id":"run-1","conclusion":"success"}`),
	})
	if err != nil {
		t.Fatalf("enqueue generic: %v", err)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_mailbox_message WHERE id = $1`, generic.ID)

	claim := func(sessionFilter string) []daemonDeliveryClaim {
		t.Helper()
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/daemon/deliveries/claim", map[string]any{"limit": 10})
		ctx := middleware.WithDaemonContext(r.Context(), testWorkspaceID, "daemon-unified-worker")
		ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
		testHandler.ClaimDaemonDeliveries(w, r.WithContext(ctx))
		if w.Code != http.StatusOK {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Deliveries []daemonDeliveryClaim `json:"deliveries"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode claim: %v", err)
		}
		if sessionFilter == "" {
			return resp.Deliveries
		}
		var filtered []daemonDeliveryClaim
		for _, item := range resp.Deliveries {
			if item.ExternalSession.SessionID == sessionFilter {
				filtered = append(filtered, item)
			}
		}
		return filtered
	}

	deliveries := claim("")
	byID := map[string]daemonDeliveryClaim{}
	for _, item := range deliveries {
		byID[item.ID] = item
	}
	commentItem, ok := byID[comment.ID]
	if !ok {
		t.Fatalf("unified claim did not lease the comment delivery: %+v", deliveries)
	}
	if commentItem.IssueID != issueID || commentItem.CommentID != commentID || commentItem.SourceType != "comment" {
		t.Fatalf("comment claim refs = %+v, want the issue/comment ids", commentItem)
	}
	if commentItem.Kind != "comment_mention" || commentItem.WorkspaceID != testWorkspaceID {
		t.Fatalf("comment claim kind/workspace = %q/%q, want comment_mention/%q", commentItem.Kind, commentItem.WorkspaceID, testWorkspaceID)
	}
	genericItem, ok := byID[generic.ID]
	if !ok {
		t.Fatalf("unified claim did not lease the generic delivery: %+v", deliveries)
	}
	if genericItem.IssueID != "" || genericItem.CommentID != "" || genericItem.SourceType != "ci_run" {
		t.Fatalf("generic claim refs = %+v, want no fabricated issue/comment", genericItem)
	}
	if genericItem.WorkspaceID != testWorkspaceID {
		t.Fatalf("generic claim workspace = %q, want %q", genericItem.WorkspaceID, testWorkspaceID)
	}
	if commentItem.VerifyOnly || genericItem.VerifyOnly {
		t.Fatalf("first claim marked a fresh delivery verification-only: %+v", deliveries)
	}

	outcome := func(item daemonDeliveryClaim, state, diagnostic string) int {
		t.Helper()
		body := map[string]any{
			"delivery_token": item.DeliveryToken,
			"outcome":        state,
		}
		if diagnostic != "" {
			body["error"] = diagnostic
		}
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/daemon/deliveries/"+item.ID+"/outcome", body)
		ctx := middleware.WithDaemonContext(r.Context(), testWorkspaceID, "daemon-unified-worker")
		ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
		r = r.WithContext(ctx)
		r = withURLParam(r, "deliveryId", item.ID)
		testHandler.RecordDaemonDeliveryOutcome(w, r)
		return w.Code
	}
	if code := outcome(genericItem, "delivered", ""); code != http.StatusOK {
		t.Fatalf("delivered outcome = %d", code)
	}
	if code := outcome(genericItem, "delivered", ""); code != http.StatusConflict {
		t.Fatalf("replayed outcome status = %d, want 409", code)
	}
	if code := outcome(genericItem, "delivered", "unexpected error"); code != http.StatusBadRequest {
		t.Fatalf("delivered-with-error status = %d, want 400", code)
	}

	stored, err := store.Get(t.Context(), generic.Scope, generic.ID)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if stored.DeliveryStatus != "delivered" || stored.DeliveredAt == nil {
		t.Fatalf("stored delivery = %+v, want delivered", stored)
	}

	// An expired comment lease is ambiguous: the server itself marks it
	// verification-only, so the worker looks up the receipt instead of sending
	// the comment a second time.
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, comment.ID)
	reclaimed := claim("")
	reclaimedByID := map[string]daemonDeliveryClaim{}
	for _, item := range reclaimed {
		reclaimedByID[item.ID] = item
	}
	reclaimedComment, ok := reclaimedByID[comment.ID]
	if !ok || !reclaimedComment.VerifyOnly {
		t.Fatalf("reclaimed comment = %+v, want verify_only", reclaimedComment)
	}
}

// TestGenericDeliveryOutcomeValidation pins the generic outcome vocabulary,
// including the diagnostic required for an unconfirmed result.
func TestGenericDeliveryOutcomeValidation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name     string
		body     map[string]any
		wantCode int
	}{
		{name: "unknown without diagnostic", body: map[string]any{"delivery_token": "t", "outcome": "unknown"}, wantCode: http.StatusBadRequest},
		{name: "bad outcome", body: map[string]any{"delivery_token": "t", "outcome": "maybe"}, wantCode: http.StatusBadRequest},
		{name: "bad retry class", body: map[string]any{"delivery_token": "t", "outcome": "retry", "retry_class": "whenever"}, wantCode: http.StatusBadRequest},
		{name: "missing token", body: map[string]any{"outcome": "delivered"}, wantCode: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := newRequest(http.MethodPost, "/api/daemon/deliveries/x/outcome", tc.body)
			ctx := middleware.WithDaemonContext(r.Context(), testWorkspaceID, "daemon-generic-worker")
			r = r.WithContext(ctx)
			r = withURLParam(r, "deliveryId", "01a1713c-3ad1-7157-b4eb-3cfeb67d152f")
			testHandler.RecordDaemonDeliveryOutcome(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}
