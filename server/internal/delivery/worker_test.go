package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordedOutcome struct {
	workspaceID string
	request     OutcomeRequest
}

type fakeOutcomeClient struct {
	mu sync.Mutex

	claims   map[string][][]Claim
	claimErr map[string]error

	acks        []recordedOutcome
	ackErrs     []error
	nextAttempt *time.Time
}

func newFakeOutcomeClient() *fakeOutcomeClient {
	return &fakeOutcomeClient{claims: map[string][][]Claim{}, claimErr: map[string]error{}}
}

func (f *fakeOutcomeClient) ClaimDeliveries(_ context.Context, workspaceID string, _ int) ([]Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.claimErr[workspaceID]; err != nil {
		return nil, err
	}
	batches := f.claims[workspaceID]
	if len(batches) == 0 {
		return nil, nil
	}
	f.claims[workspaceID] = batches[1:]
	return batches[0], nil
}

func (f *fakeOutcomeClient) RecordDeliveryOutcome(_ context.Context, workspaceID string, req OutcomeRequest) (Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, recordedOutcome{workspaceID: workspaceID, request: req})
	if len(f.ackErrs) > 0 {
		err := f.ackErrs[0]
		f.ackErrs = f.ackErrs[1:]
		if err != nil {
			return Delivery{}, err
		}
	}
	return Delivery{ID: req.DeliveryID, DeliveryStatus: req.storedStatus(), NextAttemptAt: f.nextAttempt}, nil
}

func TestWorkerLogsAttemptWithoutPayloadOrCredentials(t *testing.T) {
	next := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	client := newFakeOutcomeClient()
	client.nextAttempt = &next
	claim := testClaim("delivery-1", "session-1", "secret-token")
	claim.Attempts = 42
	claim.Payload = []byte("private payload")
	host := newFakeHost()
	host.deliverErrs[claim.ID] = &HostError{Kind: HostBusy, Code: "agent_busy", Message: "private payload bearer secret"}
	var logs bytes.Buffer
	worker := NewWorker(client, host, nil, "daemon-1", slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := worker.handleClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"level": "INFO", "delivery_id": "delivery-1", "kind": "ci_terminal", "session_id": "session-1", "attempt": float64(42), "result": "busy", "next_attempt_at": "2026-01-02T03:04:05Z", "reason": "session busy"} {
		if entry[key] != want {
			t.Errorf("%s = %v, want %v", key, entry[key], want)
		}
	}
	if strings.Contains(logs.String(), "private payload") || strings.Contains(logs.String(), "secret") {
		t.Fatalf("sensitive data in log: %s", logs.String())
	}
}

func TestWorkerLogsScheduleAfterDelayedAcknowledgement(t *testing.T) {
	next := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	client := newFakeOutcomeClient()
	client.nextAttempt = &next
	client.ackErrs = []error{errors.New("temporary failure")}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostBusy}
	var logs bytes.Buffer
	worker := NewWorker(client, host, nil, "daemon-1", slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := worker.handleClaim(context.Background(), testClaim("delivery-1", "session-1", "lease-1")); err != nil {
		t.Fatal(err)
	}
	worker.retryPending(context.Background())
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logs = %s", logs.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["delivery_id"] != "delivery-1" || entry["next_attempt_at"] != "2026-01-02T03:04:05.123Z" {
		t.Fatalf("retry log = %v", entry)
	}
}

func (f *fakeOutcomeClient) ackSnapshot() []recordedOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedOutcome(nil), f.acks...)
}

type fakeHost struct {
	mu sync.Mutex

	deliveries   []string
	deliverErrs  map[string]error
	receipts     map[string]Receipt
	receiptErrs  map[string]error
	receiptCalls int

	inFlight    int32
	maxInFlight int32
	entered     chan string
	release     chan struct{}
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		deliverErrs: map[string]error{},
		receipts:    map[string]Receipt{},
		receiptErrs: map[string]error{},
	}
}

func (f *fakeHost) DeliverIfIdle(ctx context.Context, target HostTarget, d Delivery) (string, error) {
	current := atomic.AddInt32(&f.inFlight, 1)
	for {
		max := atomic.LoadInt32(&f.maxInFlight)
		if current <= max || atomic.CompareAndSwapInt32(&f.maxInFlight, max, current) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)

	f.mu.Lock()
	f.deliveries = append(f.deliveries, d.ID)
	err := f.deliverErrs[d.ID]
	f.mu.Unlock()

	if f.entered != nil {
		f.entered <- d.ID
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
		}
	}
	if err != nil {
		return "", err
	}
	return "paseo:" + target.SessionID + ":" + d.ID, nil
}

func (f *fakeHost) LookupReceipt(_ context.Context, _ HostTarget, deliveryID string) (Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receiptCalls++
	if err := f.receiptErrs[deliveryID]; err != nil {
		return Receipt{}, err
	}
	receipt, ok := f.receipts[deliveryID]
	if !ok {
		return Receipt{Status: ReceiptAbsent}, nil
	}
	return receipt, nil
}

func (f *fakeHost) deliverCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deliveries)
}

func testClaim(id, sessionID, token string) Claim {
	recipient := Recipient{Type: "agent", ID: "agent-" + sessionID}
	return Claim{
		Delivery: Delivery{
			ID:             id,
			Scope:          Scope{WorkspaceID: "ws-1"},
			Recipient:      recipient,
			Kind:           "ci_terminal",
			Payload:        []byte(`{"run_id":"run-42"}`),
			DeliveryStatus: "leased",
		},
		Session: HostTarget{
			Recipient: recipient,
			Provider:  "paseo",
			HostID:    "host-1",
			SessionID: sessionID,
			DaemonID:  "daemon-1",
		},
		LeaseToken:     token,
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}
}

func newTestWorker(client *fakeOutcomeClient, host *fakeHost) *Worker {
	return NewWorker(client, host, func() []string { return []string{"ws-1"} }, "daemon-1", nil)
}

func TestWorkerDeliversIdleAndRecordsAcceptedReceipt(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	if host.deliverCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", host.deliverCount())
	}
	acks := client.ackSnapshot()
	if len(acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(acks))
	}
	if acks[0].request.State != OutcomeDelivered || acks[0].request.LeaseToken != "lease-1" {
		t.Fatalf("ack = %+v", acks[0].request)
	}
	if acks[0].request.HostReceipt != "paseo:session-1:delivery-1" {
		t.Fatalf("host receipt = %q", acks[0].request.HostReceipt)
	}
	if len(worker.pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(worker.pending))
	}
}

// A busy session is healthy: the request was not accepted, so the same id is
// retried with a short delay instead of the failure backoff.
func TestWorkerBusyRetriesWithSameDeliveryID(t *testing.T) {
	client := newFakeOutcomeClient()
	claim := testClaim("delivery-1", "session-1", "lease-1")
	claim.VerifyOnly = false
	second := testClaim("delivery-1", "session-1", "lease-2")
	client.claims["ws-1"] = [][]Claim{{claim}, {second}}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostBusy, Code: "agent_busy"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())
	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeRetry || acks[0].request.RetryClass != RetryBusy {
		t.Fatalf("busy ack = %+v", acks)
	}

	// The busy result is a known no-effect outcome, so the row can be delivered
	// again under a new lease with the original delivery id.
	delete(host.deliverErrs, "delivery-1")
	worker.round(context.Background())
	acks = client.ackSnapshot()
	if len(acks) != 2 || acks[1].request.State != OutcomeDelivered {
		t.Fatalf("second ack = %+v", acks)
	}
	if len(host.deliveries) != 2 || host.deliveries[0] != "delivery-1" || host.deliveries[1] != "delivery-1" {
		t.Fatalf("deliveries = %v, want the same id twice", host.deliveries)
	}
}

func TestWorkerUnavailableUsesFailureBackoffWithoutChangingID(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostUnavailable, Code: "DAEMON_UNREACHABLE"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeRetry || acks[0].request.RetryClass != RetryUnavailable {
		t.Fatalf("ack = %+v", acks)
	}
	if !contains(acks[0].request.Error, "DAEMON_UNREACHABLE") {
		t.Fatalf("error = %q, want the host code", acks[0].request.Error)
	}
}

// A host without atomic if-idle delivery has no fallback path: the request stays
// queued with a visible dependency error.
func TestWorkerDoesNotFallBackWhenIdleDeliveryIsUnsupported(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostUnsupported, Code: "paseo_dependency_unsupported", Message: "the installed paseo CLI does not support --if-idle"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeRetry || acks[0].request.RetryClass != RetryDependency {
		t.Fatalf("ack = %+v", acks)
	}
	if host.deliverCount() != 1 {
		t.Fatalf("host deliveries = %d, want exactly the one rejected attempt", host.deliverCount())
	}
	if !contains(acks[0].request.Error, "does not support") {
		t.Fatalf("error = %q, want the capability limitation", acks[0].request.Error)
	}
}

func TestWorkerRecordsExplicitRefusalAsTerminalFailure(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostRejected, Code: "AGENT_NOT_FOUND", Message: "the registered session no longer exists"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeRejected {
		t.Fatalf("ack = %+v", acks)
	}
	if acks[0].request.storedStatus() != "failed" {
		t.Fatalf("stored status = %q, want failed", acks[0].request.storedStatus())
	}
}

// An ambiguous result is resolved by the host's read-only receipt. Accepted is
// the only result that completes the delivery.
func TestWorkerVerifiesUnknownWithAcceptedReceipt(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostOutcomeUnknown, Code: "delivery_timeout"}
	host.receipts["delivery-1"] = Receipt{Status: ReceiptAccepted, Reference: "paseo:session-1:delivery-1:accepted"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	if host.deliverCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", host.deliverCount())
	}
	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeDelivered {
		t.Fatalf("ack = %+v", acks)
	}
	if acks[0].request.HostReceipt != "paseo:session-1:delivery-1:accepted" {
		t.Fatalf("host receipt = %q", acks[0].request.HostReceipt)
	}
}

// pending and absent are both "not confirmed". Neither may turn into a replay
// with a new id or a cleared receipt.
func TestWorkerKeepsUnknownWithoutReplay(t *testing.T) {
	for _, status := range []ReceiptStatus{ReceiptPending, ReceiptAbsent} {
		t.Run(string(status), func(t *testing.T) {
			client := newFakeOutcomeClient()
			first := testClaim("delivery-1", "session-1", "lease-1")
			client.claims["ws-1"] = [][]Claim{{first}}
			host := newFakeHost()
			host.deliverErrs["delivery-1"] = &HostError{Kind: HostOutcomeUnknown, Code: "delivery_timeout"}
			host.receipts["delivery-1"] = Receipt{Status: status}
			worker := newTestWorker(client, host)

			worker.round(context.Background())

			acks := client.ackSnapshot()
			if len(acks) != 1 || acks[0].request.State != OutcomeUnknown {
				t.Fatalf("ack = %+v", acks)
			}
			if acks[0].request.DeliveryID != "delivery-1" {
				t.Fatalf("delivery id changed to %q", acks[0].request.DeliveryID)
			}
			if !contains(acks[0].request.Error, "not proof of non-execution") && !contains(acks[0].request.Error, "may have started") {
				t.Fatalf("error = %q, want an explicit unconfirmed reason", acks[0].request.Error)
			}

			// The next claim is verification-only: the host is asked for the
			// receipt again but the message is never sent a second time.
			recheck := testClaim("delivery-1", "session-1", "lease-2")
			recheck.VerifyOnly = true
			client.claims["ws-1"] = [][]Claim{{recheck}}
			worker.round(context.Background())
			if host.deliverCount() != 1 {
				t.Fatalf("host deliveries = %d after recheck, want 1", host.deliverCount())
			}
			if host.receiptCalls != 2 {
				t.Fatalf("receipt lookups = %d, want 2", host.receiptCalls)
			}
		})
	}
}

func TestWorkerUnknownWithoutReceiptCapabilityStaysUnconfirmed(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	host.deliverErrs["delivery-1"] = &HostError{Kind: HostOutcomeUnknown, Code: "delivery_timeout"}
	host.receiptErrs["delivery-1"] = &HostError{Kind: HostUnsupported, Code: "DAEMON_UPDATE_REQUIRED"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeUnknown {
		t.Fatalf("ack = %+v", acks)
	}
	if !contains(acks[0].request.Error, "cannot look up delivery receipts") {
		t.Fatalf("error = %q, want the capability limitation", acks[0].request.Error)
	}
	if host.deliverCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", host.deliverCount())
	}
}

// An expired lease means the previous holder disappeared without recording an
// outcome. The reclaim must verify, not resend.
func TestWorkerReclaimedLeaseVerifiesInsteadOfResending(t *testing.T) {
	client := newFakeOutcomeClient()
	claim := testClaim("delivery-1", "session-1", "lease-2")
	claim.VerifyOnly = true
	client.claims["ws-1"] = [][]Claim{{claim}}
	host := newFakeHost()
	host.receipts["delivery-1"] = Receipt{Status: ReceiptAccepted, Reference: "receipt-1"}
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	if host.deliverCount() != 0 {
		t.Fatalf("deliveries = %d, want 0 for a reclaimed lease", host.deliverCount())
	}
	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeDelivered {
		t.Fatalf("ack = %+v", acks)
	}
}

func TestWorkerRetriesDelayedAckWithoutSecondDelivery(t *testing.T) {
	client := newFakeOutcomeClient()
	client.ackErrs = []error{errors.New("network unavailable")}
	claim := testClaim("delivery-1", "session-1", "lease-1")
	client.claims["ws-1"] = [][]Claim{{claim}}
	host := newFakeHost()
	worker := newTestWorker(client, host)

	worker.round(context.Background())
	if host.deliverCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", host.deliverCount())
	}
	if len(worker.pending) != 1 {
		t.Fatalf("pending = %d, want the delayed ack to stay pending", len(worker.pending))
	}

	worker.round(context.Background())
	if host.deliverCount() != 1 {
		t.Fatalf("deliveries after ack retry = %d, want 1", host.deliverCount())
	}
	acks := client.ackSnapshot()
	if len(acks) != 2 || acks[1].request.State != OutcomeDelivered {
		t.Fatalf("acks = %+v", acks)
	}
	if len(worker.pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(worker.pending))
	}
}

// A stale lease ack keeps the decided outcome until the row is reclaimed, then
// re-records it under the new lease instead of sending again.
func TestWorkerStaleAckWaitsForNewLeaseWithoutResending(t *testing.T) {
	client := newFakeOutcomeClient()
	client.ackErrs = []error{fmt.Errorf("%w: conflict", ErrLeaseLost)}
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	worker := newTestWorker(client, host)

	worker.round(context.Background())
	if host.deliverCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", host.deliverCount())
	}

	reclaim := testClaim("delivery-1", "session-1", "lease-2")
	reclaim.VerifyOnly = true
	client.claims["ws-1"] = [][]Claim{{reclaim}}
	worker.round(context.Background())

	if host.deliverCount() != 1 {
		t.Fatalf("deliveries after reclaim = %d, want 1", host.deliverCount())
	}
	acks := client.ackSnapshot()
	last := acks[len(acks)-1]
	if last.request.LeaseToken != "lease-2" || last.request.State != OutcomeDelivered {
		t.Fatalf("re-acked outcome = %+v", last.request)
	}
	if len(worker.pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(worker.pending))
	}
}

// One revoked or unreachable workspace must not stop the daemon's other
// workspaces from being served.
func TestWorkerSkipsFailingWorkspaceAndServesOthers(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claimErr["ws-bad"] = errors.New("403 forbidden")
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	worker := NewWorker(client, host, func() []string { return []string{"ws-bad", "ws-1"} }, "daemon-1", nil)

	worker.round(context.Background())

	if host.deliverCount() != 1 {
		t.Fatalf("deliveries = %d, want 1", host.deliverCount())
	}
	if acks := client.ackSnapshot(); len(acks) != 1 || acks[0].workspaceID != "ws-1" {
		t.Fatalf("acks = %+v", acks)
	}
}

func TestWorkerBoundsConcurrencyAcrossSessions(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{
		testClaim("delivery-1", "session-1", "lease-1"),
		testClaim("delivery-2", "session-2", "lease-2"),
		testClaim("delivery-3", "session-3", "lease-3"),
	}}
	host := newFakeHost()
	host.entered = make(chan string, 3)
	host.release = make(chan struct{})
	worker := newTestWorker(client, host)
	worker.concurrency = 2

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.round(context.Background())
	}()

	// Two sessions start; the third waits for a slot.
	<-host.entered
	<-host.entered
	select {
	case id := <-host.entered:
		t.Fatalf("third delivery %s started before a slot was free", id)
	case <-time.After(50 * time.Millisecond):
	}
	close(host.release)
	<-done

	if host.deliverCount() != 3 {
		t.Fatalf("deliveries = %d, want 3", host.deliverCount())
	}
	if max := atomic.LoadInt32(&host.maxInFlight); max > 2 {
		t.Fatalf("max concurrent deliveries = %d, want <= 2", max)
	}
}

// A malformed batch that repeats one session is reduced to its first row: the
// same session must never receive overlapping turns. The dropped row is still
// acknowledged as a retry so it does not become an unverifiable unknown.
func TestWorkerNeverRunsOneSessionTwiceInABatch(t *testing.T) {
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{
		testClaim("delivery-1", "session-1", "lease-1"),
		testClaim("delivery-2", "session-1", "lease-2"),
	}}
	host := newFakeHost()
	worker := newTestWorker(client, host)

	worker.round(context.Background())

	if host.deliverCount() != 1 || host.deliveries[0] != "delivery-1" {
		t.Fatalf("deliveries = %v, want only the first row for the session", host.deliveries)
	}
	byID := map[string]OutcomeRequest{}
	for _, ack := range client.ackSnapshot() {
		byID[ack.request.DeliveryID] = ack.request
	}
	delivered, ok := byID["delivery-1"]
	if !ok || delivered.State != OutcomeDelivered {
		t.Fatalf("delivery-1 ack = %+v, want delivered", delivered)
	}
	dropped, ok := byID["delivery-2"]
	if !ok || dropped.State != OutcomeRetry || dropped.LeaseToken != "lease-2" {
		t.Fatalf("delivery-2 ack = %+v, want a retry under its own lease", dropped)
	}
}

// A claim the worker cannot use proves the host was never called for that
// lease. It must be recorded immediately; returning an error would leave the
// row leased until verification read an absent receipt and parked a never-sent
// message as permanently unknown. A verification-only row never returns to
// claimable delivery, because its earlier attempt may already have been
// accepted.
func TestWorkerRecordsUnusableClaimWithoutCallingHost(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Claim)
		want       string
		wantState  OutcomeState
		wantClass  RetryClass
		wantStatus string
	}{
		{
			name:       "missing host session",
			mutate:     func(c *Claim) { c.Session.SessionID = "" },
			want:       "no host session id",
			wantState:  OutcomeRetry,
			wantClass:  RetryDependency,
			wantStatus: "pending",
		},
		{
			name:       "foreign daemon",
			mutate:     func(c *Claim) { c.Session.DaemonID = "daemon-2" },
			want:       "bound to daemon",
			wantState:  OutcomeRetry,
			wantClass:  RetryDependency,
			wantStatus: "pending",
		},
		{
			name:       "unparseable lease",
			mutate:     func(c *Claim) { c.ClaimError = "invalid lease_expires_at" },
			want:       "invalid lease_expires_at",
			wantState:  OutcomeRetry,
			wantClass:  RetryDependency,
			wantStatus: "pending",
		},
		{
			name: "unparseable lease on a verification-only reclaim",
			mutate: func(c *Claim) {
				c.ClaimError = "invalid lease_expires_at"
				c.VerifyOnly = true
			},
			want:       "not replayed",
			wantState:  OutcomeUnknown,
			wantStatus: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeOutcomeClient()
			claim := testClaim("delivery-1", "session-1", "lease-1")
			tc.mutate(&claim)
			client.claims["ws-1"] = [][]Claim{{claim}}
			host := newFakeHost()
			worker := newTestWorker(client, host)

			worker.round(context.Background())

			if host.deliverCount() != 0 || host.receiptCalls != 0 {
				t.Fatalf("host was called (deliveries=%d receipts=%d) for an unusable claim", host.deliverCount(), host.receiptCalls)
			}
			acks := client.ackSnapshot()
			if len(acks) != 1 || acks[0].request.State != tc.wantState || acks[0].request.RetryClass != tc.wantClass {
				t.Fatalf("ack = %+v, want state %q class %q", acks, tc.wantState, tc.wantClass)
			}
			if acks[0].request.storedStatus() != tc.wantStatus {
				t.Fatalf("stored status = %q, want %q", acks[0].request.storedStatus(), tc.wantStatus)
			}
			if !contains(acks[0].request.Error, tc.want) {
				t.Fatalf("error = %q, want %q", acks[0].request.Error, tc.want)
			}
			if len(worker.pending) != 0 {
				t.Fatalf("pending = %d, want the immediate ack to be recorded", len(worker.pending))
			}
		})
	}
}

// An unacknowledged delivered result must survive a later unusable claim of the
// same row: the new lease only changes which token the ack uses.
func TestWorkerUnusableClaimPreservesPendingDeliveredOutcome(t *testing.T) {
	client := newFakeOutcomeClient()
	client.ackErrs = []error{errors.New("network unavailable"), errors.New("network unavailable")}
	client.claims["ws-1"] = [][]Claim{{testClaim("delivery-1", "session-1", "lease-1")}}
	host := newFakeHost()
	worker := newTestWorker(client, host)

	worker.round(context.Background())
	if host.deliverCount() != 1 || len(worker.pending) != 1 {
		t.Fatalf("after delivery: deliveries=%d pending=%d, want 1/1", host.deliverCount(), len(worker.pending))
	}

	// The lease expires and is reclaimed, but the new claim is unusable. The
	// locally decided delivered result must be re-acked, not overwritten.
	reclaim := testClaim("delivery-1", "session-1", "lease-2")
	reclaim.VerifyOnly = true
	reclaim.ClaimError = "invalid lease_expires_at"
	client.claims["ws-1"] = [][]Claim{{reclaim}}
	worker.round(context.Background())

	if host.deliverCount() != 1 || host.receiptCalls != 0 {
		t.Fatalf("host was called again (deliveries=%d receipts=%d)", host.deliverCount(), host.receiptCalls)
	}
	acks := client.ackSnapshot()
	last := acks[len(acks)-1]
	if last.request.State != OutcomeDelivered || last.request.LeaseToken != "lease-2" {
		t.Fatalf("last ack = %+v, want delivered under the new lease", last.request)
	}
	if len(worker.pending) != 0 {
		t.Fatalf("pending = %d, want the decided outcome acknowledged", len(worker.pending))
	}
}

func TestVerifyOnlyCoversEverySource(t *testing.T) {
	for _, status := range []string{"leased", "unknown", ""} {
		if !verifyOnly(status, false) {
			t.Fatalf("status %q must be verification-only: the host may have accepted it", status)
		}
	}
	if verifyOnly("pending", false) {
		t.Fatal("a known no-effect row is deliverable again")
	}
	// A cancellation can commit after the candidate snapshot; the leased row's
	// marker wins over a stale "pending" candidate.
	if !verifyOnly("pending", true) {
		t.Fatal("a withdrawn notification was not verification-only")
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
