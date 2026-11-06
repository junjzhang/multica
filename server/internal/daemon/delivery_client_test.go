package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One malformed lease timestamp must not discard the rest of an already-leased
// batch. The bad claim is returned with its token and a diagnostic so the
// worker can record a retry; the caller never called the host for it.
func TestClaimDeliveriesKeepsBatchAndFlagsOneInvalidLeaseTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/deliveries/claim" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"deliveries":[
			{"id":"delivery-1","workspace_id":"ws-1","kind":"ci_terminal","payload":{"run_id":"run-1"},
			 "recipient_agent_id":"agent-1","recipient_type":"agent",
			 "external_session":{"provider":"paseo","host_id":"host-1","session_id":"session-1","daemon_id":"daemon-1"},
			 "delivery_status":"leased","verify_only":false,"delivery_token":"lease-1",
			 "lease_expires_at":"2026-10-23T09:35:50Z"},
			{"id":"delivery-2","workspace_id":"ws-1","kind":"ci_terminal","payload":{"run_id":"run-2"},
			 "recipient_agent_id":"agent-1","recipient_type":"agent",
			 "external_session":{"provider":"paseo","host_id":"host-1","session_id":"session-1","daemon_id":"daemon-1"},
			 "delivery_status":"leased","verify_only":true,"delivery_token":"lease-2",
			 "lease_expires_at":"not-a-timestamp"}
		]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("daemon-token")
	client.mailboxDaemonID = "daemon-1"

	claims, err := client.ClaimDeliveries(context.Background(), "ws-1", 5)
	if err != nil {
		t.Fatalf("ClaimDeliveries: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %d, want both leases", len(claims))
	}
	if claims[0].ID != "delivery-1" || claims[0].ClaimError != "" {
		t.Fatalf("valid claim = %+v", claims[0])
	}
	wantExpiry := time.Date(2026, 10, 23, 9, 35, 50, 0, time.UTC)
	if !claims[0].LeaseExpiresAt.Equal(wantExpiry) {
		t.Fatalf("lease expiry = %v, want %v", claims[0].LeaseExpiresAt, wantExpiry)
	}
	bad := claims[1]
	if bad.ID != "delivery-2" || bad.LeaseToken != "lease-2" || !bad.VerifyOnly {
		t.Fatalf("invalid claim = %+v, want the original id, token and verify flag", bad)
	}
	if !strings.Contains(bad.ClaimError, "lease_expires_at") {
		t.Fatalf("invalid claim error = %q", bad.ClaimError)
	}
}

// The claim DTO is the only place the frozen comment envelope's identifiers are
// read. A wrong json tag would silently drop them while every byte-for-byte
// template test still passed, so this fixture goes DTO -> delivery.Claim ->
// renderer and compares the complete envelope.
func TestClaimDeliveriesMapsStoredRowAndRendersCommentEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"deliveries":[{
			"id":"delivery-comment-1",
			"workspace_id":"workspace-row",
			"kind":"comment_mention",
			"payload":{"schema":"comment_mention.v1","issue_id":"issue-1","comment_id":"comment-1"},
			"source_type":"comment",
			"issue_id":"issue-1",
			"comment_id":"comment-1",
			"recipient_agent_id":"agent-1",
			"recipient_type":"agent",
			"external_session":{"provider":"paseo","host_id":"host-1","session_id":"session-1","daemon_id":"daemon-1"},
			"delivery_status":"leased",
			"attempts":1,
			"verify_only":false,
			"delivery_token":"lease-1",
			"lease_expires_at":"2026-10-23T09:35:50Z"
		}]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("daemon-token")
	client.mailboxDaemonID = "daemon-1"

	claims, err := client.ClaimDeliveries(context.Background(), "workspace-row", 5)
	if err != nil {
		t.Fatalf("ClaimDeliveries: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	claim := claims[0]
	if claim.ClaimError != "" {
		t.Fatalf("claim error = %q, want a usable lease", claim.ClaimError)
	}
	if claim.Scope.WorkspaceID != "workspace-row" {
		t.Fatalf("claim workspace = %q, want the stored row's workspace", claim.Scope.WorkspaceID)
	}
	if claim.Source.Type != "comment" || claim.Kind != "comment_mention" {
		t.Fatalf("source/kind = %q/%q, want comment/comment_mention", claim.Source.Type, claim.Kind)
	}
	if claim.Refs.IssueID != "issue-1" || claim.Refs.CommentID != "comment-1" {
		t.Fatalf("refs = %+v, want the issue/comment ids from the DTO", claim.Refs)
	}

	want := "Multica has a request for this session.\n" +
		"Mailbox ID: delivery-comment-1\n" +
		"Workspace ID: workspace-row\n" +
		"Issue ID: issue-1\n" +
		"Comment ID: comment-1\n" +
		"Recipient agent ID: agent-1\n\n" +
		"Use this connected Paseo session's CLI identity without --profile. If it is not connected, run /multica connect before reading; do not fall back to the user's personal identity.\n" +
		"Read: multica issue get issue-1 --output json\n" +
		"Read comments: multica issue comment list issue-1 --output json\n" +
		"This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone."
	if got := renderDeliveryNotification(claim.Delivery); got != want {
		t.Fatalf("comment envelope changed:\n%s", got)
	}
}

// A claim response without the stored row's workspace, or echoing a different
// one, must not become a valid lease: that would render an envelope whose
// frozen workspace is not the row the token covers.
func TestClaimDeliveriesRejectsMissingOrForeignWorkspaceEcho(t *testing.T) {
	cases := []struct {
		name          string
		workspaceJSON string
	}{
		{name: "missing workspace_id", workspaceJSON: ""},
		{name: "foreign workspace_id", workspaceJSON: `"workspace_id":"workspace-other",`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"deliveries":[{` + tc.workspaceJSON + `
					"id":"delivery-1",
					"kind":"comment_mention",
					"payload":{"schema":"comment_mention.v1"},
					"source_type":"comment",
					"issue_id":"issue-1",
					"comment_id":"comment-1",
					"recipient_agent_id":"agent-1",
					"recipient_type":"agent",
					"external_session":{"provider":"paseo","host_id":"host-1","session_id":"session-1","daemon_id":"daemon-1"},
					"delivery_status":"leased",
					"verify_only":false,
					"delivery_token":"lease-1",
					"lease_expires_at":"2026-10-23T09:35:50Z"
				}]}`))
			}))
			defer srv.Close()
			client := NewClient(srv.URL)
			client.SetToken("daemon-token")
			client.mailboxDaemonID = "daemon-1"

			claims, err := client.ClaimDeliveries(context.Background(), "workspace-1", 5)
			if err != nil {
				t.Fatalf("ClaimDeliveries: %v", err)
			}
			if len(claims) != 1 {
				t.Fatalf("claims = %d, want the unusable lease reported", len(claims))
			}
			claim := claims[0]
			if !strings.Contains(claim.ClaimError, "workspace") {
				t.Fatalf("claim error = %q, want a workspace diagnostic", claim.ClaimError)
			}
			if claim.Scope.WorkspaceID != "workspace-1" {
				t.Fatalf("claim scope = %q, want the claimed workspace for the outcome ack", claim.Scope.WorkspaceID)
			}
		})
	}
}
