package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/middleware"
)

// TestGenericDeliveryClaimAndOutcome pins the generic consumer contract: it
// leases only non-comment rows, exposes no issue/comment fields, fences the
// outcome by the lease token, and marks a reclaimed lease verify_only instead
// of making the row deliverable again.
func TestGenericDeliveryClaimAndOutcome(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, ownerID := externalAgentFixture(t, "Generic Delivery Target", "daemon-generic-worker")
	created, err := delivery.NewPostgres(testHandler.Queries).Enqueue(t.Context(), delivery.EnqueueRequest{
		Scope:     delivery.Scope{WorkspaceID: testWorkspaceID},
		Source:    delivery.Source{Type: "ci_run", Key: "generic-claim-run-1"},
		Recipient: delivery.Recipient{Type: "agent", ID: agentID},
		Kind:      "ci_terminal",
		Payload:   json.RawMessage(`{"run_id":"run-1","conclusion":"success"}`),
	})
	if err != nil {
		t.Fatalf("enqueue generic: %v", err)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_mailbox_message WHERE id = $1`, created.ID)

	claim := func(handler func(http.ResponseWriter, *http.Request)) []map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/daemon/deliveries/claim", map[string]any{"limit": 10})
		ctx := middleware.WithDaemonContext(r.Context(), testWorkspaceID, "daemon-generic-worker")
		ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
		handler(w, r.WithContext(ctx))
		if w.Code != http.StatusOK {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Deliveries []map[string]any `json:"deliveries"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode claim: %v", err)
		}
		return resp.Deliveries
	}

	// The frozen comment entry never sees the generic row.
	v := httptest.NewRecorder()
	vr := newRequest(http.MethodPost, "/api/daemon/mailbox/claim", map[string]any{"limit": 10})
	vctx := middleware.WithDaemonContext(vr.Context(), testWorkspaceID, "daemon-generic-worker")
	vctx = middleware.WithDaemonTokenOwnerID(vctx, ownerID)
	testHandler.ClaimDaemonMailbox(v, vr.WithContext(vctx))
	if v.Code != http.StatusOK {
		t.Fatalf("legacy claim: %d %s", v.Code, v.Body.String())
	}
	var legacy struct {
		Messages []daemonMailboxClaimItem `json:"messages"`
	}
	if err := json.NewDecoder(v.Body).Decode(&legacy); err != nil {
		t.Fatalf("decode legacy claim: %v", err)
	}
	if len(legacy.Messages) != 0 {
		t.Fatalf("legacy claim leaked a generic delivery: %+v", legacy.Messages)
	}

	deliveries := claim(testHandler.ClaimDaemonDeliveries)
	if len(deliveries) != 1 {
		t.Fatalf("generic claim = %+v, want one delivery", deliveries)
	}
	item := deliveries[0]
	for _, forbidden := range []string{"issue_id", "comment_id"} {
		if _, present := item[forbidden]; present {
			t.Fatalf("generic claim exposes %s: %+v", forbidden, item)
		}
	}
	if item["id"] != created.ID || item["kind"] != "ci_terminal" || item["verify_only"] != false {
		t.Fatalf("generic claim item = %+v", item)
	}
	token, _ := item["delivery_token"].(string)
	if token == "" {
		t.Fatal("generic claim has no delivery token")
	}

	outcome := func(token, state, diagnostic string) int {
		t.Helper()
		body := map[string]any{
			"delivery_token": token,
			"outcome":        state,
		}
		if diagnostic != "" {
			body["error"] = diagnostic
		}
		w := httptest.NewRecorder()
		r := newRequest(http.MethodPost, "/api/daemon/deliveries/"+created.ID+"/outcome", body)
		ctx := middleware.WithDaemonContext(r.Context(), testWorkspaceID, "daemon-generic-worker")
		ctx = middleware.WithDaemonTokenOwnerID(ctx, ownerID)
		r = r.WithContext(ctx)
		r = withURLParam(r, "deliveryId", created.ID)
		testHandler.RecordDaemonDeliveryOutcome(w, r)
		return w.Code
	}
	if code := outcome(token, "delivered", ""); code != http.StatusOK {
		t.Fatalf("delivered outcome = %d", code)
	}
	if code := outcome(token, "delivered", ""); code != http.StatusConflict {
		t.Fatalf("replayed outcome status = %d, want 409", code)
	}
	if code := outcome(token, "delivered", "unexpected error"); code != http.StatusBadRequest {
		t.Fatalf("delivered-with-error status = %d, want 400", code)
	}
	stored, err := delivery.NewPostgres(testHandler.Queries).Get(t.Context(), created.Scope, created.ID)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if stored.DeliveryStatus != "delivered" || stored.DeliveredAt == nil {
		t.Fatalf("stored delivery = %+v, want delivered", stored)
	}

	// An expired lease makes the next claim verification-only: the worker must
	// look up the original receipt instead of sending it again.
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET delivery_status = 'leased', lease_expires_at = now() - interval '1 minute' WHERE id = $1`, created.ID)
	reclaimed := claim(testHandler.ClaimDaemonDeliveries)
	if len(reclaimed) != 1 || reclaimed[0]["verify_only"] != true {
		t.Fatalf("reclaimed = %+v, want verify_only", reclaimed)
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
