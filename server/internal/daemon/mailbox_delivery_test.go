package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type recordedDelivery struct {
	session   ExternalSession
	messageID string
	content   string
}

type fakeMailboxTransport struct {
	deliveries []recordedDelivery
	errs       []error
	started    chan struct{}
	block      bool
}

func (f *fakeMailboxTransport) Deliver(ctx context.Context, session ExternalSession, messageID, content string) (string, error) {
	f.deliveries = append(f.deliveries, recordedDelivery{session: session, messageID: messageID, content: content})
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.block {
		<-ctx.Done()
		return "", &MailboxDeliveryError{Kind: MailboxDeliveryOutcomeUnknown, Code: "delivery_interrupted", Err: ctx.Err()}
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return "", err
		}
	}
	return "paseo:" + session.SessionID + ":" + messageID, nil
}

type recordedAck struct {
	messageID string
	ack       MailboxAckRequest
}

type fakeMailboxClient struct {
	claims          [][]MailboxClaim
	claimErrs       []error
	claimCalls      int
	claimWorkspaces []string

	ackCalls      []recordedAck
	ackWorkspaces []string
	ackErrs       []error
}

func (f *fakeMailboxClient) ClaimMailbox(_ context.Context, workspaceID string, _ int) ([]MailboxClaim, error) {
	f.claimCalls++
	f.claimWorkspaces = append(f.claimWorkspaces, workspaceID)
	if len(f.claimErrs) > 0 {
		err := f.claimErrs[0]
		f.claimErrs = f.claimErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.claims) == 0 {
		return nil, nil
	}
	claims := f.claims[0]
	f.claims = f.claims[1:]
	return claims, nil
}

func (f *fakeMailboxClient) AckMailbox(_ context.Context, workspaceID, messageID string, ack MailboxAckRequest) (*MailboxMessage, error) {
	f.ackWorkspaces = append(f.ackWorkspaces, workspaceID)
	f.ackCalls = append(f.ackCalls, recordedAck{messageID: messageID, ack: ack})
	if len(f.ackErrs) > 0 {
		err := f.ackErrs[0]
		f.ackErrs = f.ackErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return &MailboxMessage{ID: messageID}, nil
}

func testMailboxClaim(token string) MailboxClaim {
	return MailboxClaim{
		Message: MailboxMessage{
			ID:               "11111111-1111-1111-1111-111111111111",
			IssueID:          "issue-1",
			CommentID:        "comment-1",
			RecipientAgentID: "agent-1",
			DeliveryStatus:   "leased",
			ProcessingStatus: "unread",
		},
		ExternalSession: ExternalSession{
			Provider:    "paseo",
			HostID:      "host-1",
			SessionID:   "paseo-agent-1",
			DisplayName: "Reviewer",
			DaemonID:    "daemon-1",
		},
		DeliveryToken:  token,
		LeaseExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		Content:        "Please review issue WORK-1.",
		WorkspaceID:    "ws-1",
	}
}

func newTestMailboxLoop(client *fakeMailboxClient, transport *fakeMailboxTransport) *mailboxDeliveryLoop {
	return newMailboxDeliveryLoop(client, transport, func() []string { return []string{"ws-1"} }, "daemon-1", nil)
}

func TestMailboxDeliveryDeliversIdleMessageAndAcksReceipt(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{claims: [][]MailboxClaim{{claim}}}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}

	if len(transport.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(transport.deliveries))
	}
	gotDelivery := transport.deliveries[0]
	if gotDelivery.messageID != claim.Message.ID || gotDelivery.session.SessionID != claim.ExternalSession.SessionID {
		t.Fatalf("delivery = %+v", gotDelivery)
	}
	if len(client.ackCalls) != 1 {
		t.Fatalf("acks = %d, want 1", len(client.ackCalls))
	}
	ack := client.ackCalls[0]
	if ack.ack.Status != mailboxAckDelivered || ack.ack.DeliveryToken != "lease-1" {
		t.Fatalf("ack = %+v", ack)
	}
	if ack.ack.HostReceipt != "paseo:paseo-agent-1:"+claim.Message.ID {
		t.Fatalf("host_receipt = %q", ack.ack.HostReceipt)
	}
	if len(loop.pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(loop.pending))
	}
	if len(client.claimWorkspaces) != 1 || client.claimWorkspaces[0] != "ws-1" {
		t.Fatalf("claim workspaces = %v", client.claimWorkspaces)
	}
	if len(client.ackWorkspaces) != 1 || client.ackWorkspaces[0] != "ws-1" {
		t.Fatalf("ack workspaces = %v", client.ackWorkspaces)
	}
}

func TestMailboxDeliveryBusyRetriesWithStableIdempotencyKey(t *testing.T) {
	claim1 := testMailboxClaim("lease-1")
	claim2 := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{claims: [][]MailboxClaim{{claim1}, {claim2}}}
	transport := &fakeMailboxTransport{errs: []error{
		&MailboxDeliveryError{Kind: MailboxDeliveryBusy, Code: mailboxErrorAgentBusy},
		nil,
	}}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if len(client.ackCalls) != 1 || client.ackCalls[0].ack.Status != mailboxAckRetry || client.ackCalls[0].ack.Error != mailboxErrorAgentBusy {
		t.Fatalf("busy ack = %+v", client.ackCalls)
	}
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if len(client.ackCalls) != 2 || client.ackCalls[1].ack.Status != mailboxAckDelivered {
		t.Fatalf("retry ack = %+v", client.ackCalls)
	}
	if len(transport.deliveries) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(transport.deliveries))
	}
	for _, delivery := range transport.deliveries {
		if delivery.messageID != claim1.Message.ID || delivery.session.SessionID != claim1.ExternalSession.SessionID {
			t.Fatalf("retry changed host idempotency identity: %+v", delivery)
		}
	}
}

func TestMailboxDeliveryOfflineStaysQueued(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{claims: [][]MailboxClaim{{claim}}}
	transport := &fakeMailboxTransport{errs: []error{
		&MailboxDeliveryError{Kind: MailboxDeliveryUnavailable, Code: "DAEMON_UNREACHABLE"},
	}}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if len(client.ackCalls) != 1 {
		t.Fatalf("acks = %d", len(client.ackCalls))
	}
	if client.ackCalls[0].ack.Status != mailboxAckRetry || client.ackCalls[0].ack.Error != mailboxErrorHostUnreachable {
		t.Fatalf("offline ack = %+v", client.ackCalls[0])
	}
}

func TestMailboxDeliveryRejectsClaimBoundToAnotherDaemon(t *testing.T) {
	for _, daemonID := range []string{"daemon-2", ""} {
		t.Run("daemon_id="+daemonID, func(t *testing.T) {
			claim := testMailboxClaim("lease-1")
			claim.ExternalSession.DaemonID = daemonID
			client := &fakeMailboxClient{claims: [][]MailboxClaim{{claim}}}
			transport := &fakeMailboxTransport{}
			loop := newTestMailboxLoop(client, transport)

			if err := loop.deliverOnce(context.Background()); err != nil {
				t.Fatalf("deliverOnce: %v", err)
			}
			if len(transport.deliveries) != 0 {
				t.Fatalf("delivered a claim bound to another daemon: %+v", transport.deliveries)
			}
			if len(client.ackCalls) != 1 || client.ackCalls[0].ack.Status != mailboxAckRetry ||
				client.ackCalls[0].ack.Error != mailboxErrorDaemonMismatch {
				t.Fatalf("acks = %+v", client.ackCalls)
			}
		})
	}
}

func TestMailboxDeliveryDelayedAckDoesNotResend(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{
		claims:  [][]MailboxClaim{{claim}, nil},
		ackErrs: []error{&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"}},
	}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if len(loop.pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(loop.pending))
	}
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("delayed ack resent the message: deliveries = %d", len(transport.deliveries))
	}
	if len(client.ackCalls) != 2 || client.ackCalls[1].ack.Status != mailboxAckDelivered {
		t.Fatalf("acks = %+v", client.ackCalls)
	}
	if len(loop.pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(loop.pending))
	}
}

func TestMailboxDeliveryStaleTerminalAckIsReackedUnderNewLease(t *testing.T) {
	tests := []struct {
		name       string
		deliverErr error
		wantStatus string
		wantError  string
	}{
		{name: "delivered", wantStatus: mailboxAckDelivered},
		{
			name:       "ambiguous outcome",
			deliverErr: &MailboxDeliveryError{Kind: MailboxDeliveryOutcomeUnknown, Code: mailboxErrorOutcomeUnknown},
			wantStatus: mailboxAckFailed,
			wantError:  mailboxErrorOutcomeUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claim1 := testMailboxClaim("lease-1")
			claim2 := testMailboxClaim("lease-2")
			client := &fakeMailboxClient{
				claims: [][]MailboxClaim{{claim1}, {claim2}},
				ackErrs: []error{
					&requestError{StatusCode: http.StatusConflict, Body: "lease superseded"},
				},
			}
			transport := &fakeMailboxTransport{}
			if tc.deliverErr != nil {
				transport.errs = []error{tc.deliverErr}
			}
			loop := newTestMailboxLoop(client, transport)

			if err := loop.deliverOnce(context.Background()); err != nil {
				t.Fatalf("round 1: %v", err)
			}
			if len(loop.pending) != 1 {
				t.Fatalf("terminal ack was dropped after 409: pending = %d", len(loop.pending))
			}
			if pending := loop.pending[claim1.Message.ID]; !pending.awaitingLease {
				t.Fatalf("terminal ack is not waiting for a fresh lease: %+v", pending)
			}
			if err := loop.deliverOnce(context.Background()); err != nil {
				t.Fatalf("round 2: %v", err)
			}

			if len(transport.deliveries) != 1 {
				t.Fatalf("terminal outcome was replayed: deliveries = %d, want 1", len(transport.deliveries))
			}
			if len(client.ackCalls) != 2 {
				t.Fatalf("acks = %d, want 2", len(client.ackCalls))
			}
			last := client.ackCalls[len(client.ackCalls)-1].ack
			if last.DeliveryToken != "lease-2" || last.Status != tc.wantStatus {
				t.Fatalf("re-ack = %+v, want token lease-2 status %s", last, tc.wantStatus)
			}
			if tc.wantError != "" && !strings.Contains(last.Error, tc.wantError) {
				t.Fatalf("re-ack error = %q, want %q", last.Error, tc.wantError)
			}
			if len(loop.pending) != 0 {
				t.Fatalf("pending = %d, want 0", len(loop.pending))
			}
		})
	}
}

func TestMailboxDeliveryDeletedMessageDropsTerminalAck(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{
		claims:  [][]MailboxClaim{{claim}},
		ackErrs: []error{&requestError{StatusCode: http.StatusNotFound, Body: "message not found"}},
	}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if len(loop.pending) != 0 {
		t.Fatalf("deleted message kept a terminal ack forever: %+v", loop.pending)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(transport.deliveries))
	}
}

func TestMailboxDeliveryRetainsTerminalAckAcrossLongOutage(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	newClaim := testMailboxClaim("lease-2")
	now := time.Now()
	newClaim.LeaseExpiresAt = now.Add(25 * time.Hour).Format(time.RFC3339)
	client := &fakeMailboxClient{
		claims:  [][]MailboxClaim{{claim}, nil, {newClaim}},
		ackErrs: []error{&requestError{StatusCode: http.StatusConflict, Body: "lease superseded"}},
	}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)
	loop.now = func() time.Time { return now }

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if len(loop.pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(loop.pending))
	}

	loop.now = func() time.Time { return now.Add(24 * time.Hour) }
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce after retention: %v", err)
	}
	if len(loop.pending) != 1 {
		t.Fatalf("elapsed time lost a terminal outcome: %+v", loop.pending)
	}
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(transport.deliveries) != 1 || len(loop.pending) != 0 {
		t.Fatalf("reclaim must acknowledge without re-delivery: deliveries=%d pending=%d", len(transport.deliveries), len(loop.pending))
	}
}

func TestMailboxDeliveryNewLeaseSupersedesPendingAckWithoutResend(t *testing.T) {
	claim1 := testMailboxClaim("lease-1")
	claim2 := testMailboxClaim("lease-2")
	client := &fakeMailboxClient{
		claims: [][]MailboxClaim{{claim1}, {claim2}},
		ackErrs: []error{
			&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"},
			&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"},
		},
	}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("new lease replayed a delivered message: deliveries = %d", len(transport.deliveries))
	}
	if len(client.ackCalls) < 3 {
		t.Fatalf("acks = %d, want at least 3", len(client.ackCalls))
	}
	last := client.ackCalls[len(client.ackCalls)-1]
	if last.ack.DeliveryToken != "lease-2" {
		t.Fatalf("ack used stale token %q, want lease-2", last.ack.DeliveryToken)
	}
	if last.ack.Status != mailboxAckDelivered {
		t.Fatalf("ack status = %q", last.ack.Status)
	}
}

func TestMailboxDeliveryAmbiguousOutcomeIsNotReplayed(t *testing.T) {
	claim1 := testMailboxClaim("lease-1")
	claim2 := testMailboxClaim("lease-2")
	client := &fakeMailboxClient{
		claims: [][]MailboxClaim{{claim1}, {claim2}},
		ackErrs: []error{
			&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"},
			&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"},
			&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"},
		},
	}
	transport := &fakeMailboxTransport{errs: []error{
		&MailboxDeliveryError{Kind: MailboxDeliveryOutcomeUnknown, Code: mailboxErrorOutcomeUnknown},
		nil,
	}}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("ambiguous outcome was replayed: deliveries = %d", len(transport.deliveries))
	}
	foundFailed := false
	for _, call := range client.ackCalls {
		if call.ack.Status == mailboxAckFailed && strings.Contains(call.ack.Error, mailboxErrorOutcomeUnknown) {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatalf("ambiguous outcome was not marked failed: %+v", client.ackCalls)
	}
}

func TestMailboxDeliveryDependencyErrorStaysRetryable(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{claims: [][]MailboxClaim{{claim}, {testMailboxClaim("lease-2")}}}
	transport := &fakeMailboxTransport{errs: []error{
		&MailboxDeliveryError{Kind: MailboxDeliveryUnsupported, Code: "paseo_cli_unavailable", Message: "install Paseo and confirm `paseo` is on PATH"},
	}}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if len(client.ackCalls) != 1 {
		t.Fatalf("acks = %d", len(client.ackCalls))
	}
	if ack := client.ackCalls[0].ack; ack.Status != mailboxAckRetry || !strings.Contains(ack.Error, "install Paseo") {
		t.Fatalf("dependency ack = %+v, want a retry with an actionable reason", ack)
	}
	if len(loop.pending) != 0 {
		t.Fatalf("known no-effect dependency error kept a terminal ack: %+v", loop.pending)
	}

	// Installing or upgrading the dependency must let every still-pending
	// request deliver without manual cleanup.
	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if len(client.ackCalls) != 2 || client.ackCalls[1].ack.Status != mailboxAckDelivered {
		t.Fatalf("acks after upgrade = %+v", client.ackCalls)
	}
	if len(transport.deliveries) != 2 {
		t.Fatalf("deliveries = %d, want retry attempt plus delivery", len(transport.deliveries))
	}
}

func TestMailboxDeliveryExpiredLeaseDoesNotSend(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	claim.LeaseExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	client := &fakeMailboxClient{claims: [][]MailboxClaim{{claim}}}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if len(transport.deliveries) != 0 {
		t.Fatalf("expired lease was delivered: %+v", transport.deliveries)
	}
	if len(client.ackCalls) != 1 || client.ackCalls[0].ack.Error != mailboxErrorLeaseExpired {
		t.Fatalf("acks = %+v", client.ackCalls)
	}
}

func TestMailboxDeliveryStopsWithDaemonContext(t *testing.T) {
	started := make(chan struct{}, 1)
	client := &fakeMailboxClient{claims: [][]MailboxClaim{{testMailboxClaim("lease-1")}, nil, nil}}
	transport := &fakeMailboxTransport{started: started, block: true}
	loop := newTestMailboxLoop(client, transport)
	loop.pollInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("mailbox delivery never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mailbox delivery did not stop with the daemon")
	}
	if len(client.ackCalls) != 1 {
		t.Fatalf("acks = %d, want one terminal ack during stop", len(client.ackCalls))
	}
	if ack := client.ackCalls[0].ack; ack.Status != mailboxAckFailed || !strings.Contains(ack.Error, mailboxErrorOutcomeUnknown) {
		t.Fatalf("stop ack = %+v, want terminal failed outcome", ack)
	}
}

func TestMailboxDeliveryContinuesAfterWorkspaceNotFound(t *testing.T) {
	claim := testMailboxClaim("lease-2")
	claim.WorkspaceID = "ws-2"
	client := &fakeMailboxClient{
		claims:    [][]MailboxClaim{{claim}},
		claimErrs: []error{&requestError{StatusCode: http.StatusNotFound, Body: `{"error":"workspace not found"}`}},
	}
	transport := &fakeMailboxTransport{}
	loop := newMailboxDeliveryLoop(client, transport,
		func() []string { return []string{"ws-1", "ws-2"} }, "daemon-1", nil)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if len(client.claimWorkspaces) != 2 || client.claimWorkspaces[0] != "ws-1" || client.claimWorkspaces[1] != "ws-2" {
		t.Fatalf("claim workspaces = %v, want both workspaces", client.claimWorkspaces)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want the second workspace delivered", len(transport.deliveries))
	}
	if len(client.ackCalls) != 1 || client.ackCalls[0].ack.Status != mailboxAckDelivered {
		t.Fatalf("acks = %+v", client.ackCalls)
	}
	if len(client.ackWorkspaces) != 1 || client.ackWorkspaces[0] != "ws-2" {
		t.Fatalf("ack workspaces = %v", client.ackWorkspaces)
	}
}

func TestMailboxDeliveryMissingWorkspaceDoesNotStopLoop(t *testing.T) {
	client := &fakeMailboxClient{claimErrs: []error{
		&requestError{StatusCode: http.StatusNotFound, Body: `{"error":"workspace not found"}`},
	}}
	loop := newTestMailboxLoop(client, &fakeMailboxTransport{})

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if client.claimCalls != 1 {
		t.Fatalf("claims = %d, want 1", client.claimCalls)
	}
}

func TestMailboxClaimDecodesContractFields(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	raw, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MailboxClaim
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Message.ID != claim.Message.ID || decoded.ExternalSession.SessionID != claim.ExternalSession.SessionID {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestMailboxDeliveryStopFlushesDelayedAck(t *testing.T) {
	claim := testMailboxClaim("lease-1")
	client := &fakeMailboxClient{
		claims:  [][]MailboxClaim{{claim}},
		ackErrs: []error{&requestError{StatusCode: http.StatusInternalServerError, Body: "temporary"}},
	}
	transport := &fakeMailboxTransport{}
	loop := newTestMailboxLoop(client, transport)

	if err := loop.deliverOnce(context.Background()); err != nil {
		t.Fatalf("deliverOnce: %v", err)
	}
	if len(loop.pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(loop.pending))
	}
	loop.flushPendingAcksOnStop()
	if len(client.ackCalls) != 2 {
		t.Fatalf("acks = %d, want a final ack attempt on stop", len(client.ackCalls))
	}
	if len(loop.pending) != 0 {
		t.Fatalf("pending = %d after stop flush, want 0", len(loop.pending))
	}
}
