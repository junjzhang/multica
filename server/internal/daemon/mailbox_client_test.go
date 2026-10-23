package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaimMailboxUsesDaemonEndpointAndDecodesLease(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/mailbox/claim" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer daemon-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Workspace-ID") != "ws-1" {
			t.Errorf("workspace header = %q", r.Header.Get("X-Workspace-ID"))
		}
		if r.Header.Get("X-Multica-Daemon-ID") != "daemon-1" {
			t.Error("mailbox claim lost the daemon selector")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"messages":[{
			"message":{"id":"msg-1","issue_id":"issue-1","comment_id":"comment-1","recipient_agent_id":"agent-1",
				"source_session":null,"delivery_status":"leased","processing_status":"unread",
				"created_at":"2026-10-23T09:30:50Z","delivered_at":null,"last_error":null},
			"external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1",
				"display_name":"Reviewer","url":null,"daemon_id":"daemon-1"},
			"delivery_token":"lease-token","lease_expires_at":"2026-10-23T09:35:50Z",
			"content":"Please review."
		}]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("daemon-token")
	client.mailboxDaemonID = "daemon-1"

	claims, err := client.ClaimMailbox(context.Background(), "ws-1", 7)
	if err != nil {
		t.Fatalf("ClaimMailbox: %v", err)
	}
	if gotBody["limit"] != float64(7) {
		t.Fatalf("limit = %v", gotBody["limit"])
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d", len(claims))
	}
	claim := claims[0]
	if claim.Message.ID != "msg-1" || claim.DeliveryToken != "lease-token" ||
		claim.ExternalSession.SessionID != "paseo-agent-1" || claim.Content != "Please review." {
		t.Fatalf("claim = %+v", claim)
	}
	if claim.WorkspaceID != "ws-1" {
		t.Fatalf("claim workspace id = %q, want the request selector", claim.WorkspaceID)
	}
}

func TestAckMailboxUsesLeaseTokenAndClassifiesConflict(t *testing.T) {
	var got MailboxAckRequest
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/mailbox/msg-1/ack" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Multica-Daemon-ID") != "daemon-1" {
			t.Error("mailbox ack lost the daemon selector")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"id":"msg-1","delivery_status":"delivered","processing_status":"unread"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"lease superseded"}`))
		}
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("daemon-token")
	client.mailboxDaemonID = "daemon-1"

	message, err := client.AckMailbox(context.Background(), "ws-1", "msg-1", MailboxAckRequest{
		DeliveryToken: "lease-1",
		Status:        mailboxAckDelivered,
		HostReceipt:   "paseo:session:msg",
	})
	if err != nil {
		t.Fatalf("AckMailbox: %v", err)
	}
	if message.ID != "msg-1" || message.DeliveryStatus != "delivered" {
		t.Fatalf("message = %+v", message)
	}
	if got.DeliveryToken != "lease-1" || got.Status != mailboxAckDelivered || got.HostReceipt == "" {
		t.Fatalf("ack body = %+v", got)
	}

	status = http.StatusConflict
	_, err = client.AckMailbox(context.Background(), "ws-1", "msg-1", MailboxAckRequest{DeliveryToken: "old", Status: mailboxAckDelivered})
	if err == nil || !mailboxAckIsStale(err) {
		t.Fatalf("conflict err = %v, want stale lease", err)
	}
}
