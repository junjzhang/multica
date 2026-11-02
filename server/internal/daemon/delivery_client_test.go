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
			{"id":"delivery-1","kind":"ci_terminal","payload":{"run_id":"run-1"},
			 "recipient_agent_id":"agent-1","recipient_type":"agent",
			 "external_session":{"provider":"paseo","host_id":"host-1","session_id":"session-1","daemon_id":"daemon-1"},
			 "delivery_status":"leased","verify_only":false,"delivery_token":"lease-1",
			 "lease_expires_at":"2026-10-23T09:35:50Z"},
			{"id":"delivery-2","kind":"ci_terminal","payload":{"run_id":"run-2"},
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
