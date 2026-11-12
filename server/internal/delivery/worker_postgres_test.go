package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// dbWorkerClient is the worker's real server transport: it claims through the
// SQL queue and records outcomes through the same fenced write. failOutcome
// simulates a worker that never gets its acknowledgement to the server.
type dbWorkerClient struct {
	queue       *Queue
	daemonID    string
	ownerID     string
	failOutcome bool
}

func (c *dbWorkerClient) ClaimDeliveries(ctx context.Context, workspaceID string, limit int) ([]Claim, error) {
	return c.queue.Claim(ctx, ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    c.daemonID,
		OwnerUserID: c.ownerID,
		Limit:       limit,
	})
}

func (c *dbWorkerClient) RecordDeliveryOutcome(ctx context.Context, workspaceID string, req OutcomeRequest) (Delivery, error) {
	if c.failOutcome {
		return Delivery{}, errors.New("server unreachable while acknowledging the outcome")
	}
	req.Scope = Scope{WorkspaceID: workspaceID}
	return c.queue.RecordOutcome(ctx, req)
}

// workerTestHost counts host calls and returns canned receipt results. An
// accepted send and a receipt lookup are deliberately separate counters: the
// recovery contract is proven by the send count staying flat.
type workerTestHost struct {
	mu           sync.Mutex
	deliverCalls int
	lookupCalls  int
	deliveredIDs []string
	deliverErr   error
	receipt      Receipt
	receiptErr   error
}

func (h *workerTestHost) DeliverIfIdle(_ context.Context, target HostTarget, d Delivery) (string, error) {
	h.mu.Lock()
	h.deliverCalls++
	h.deliveredIDs = append(h.deliveredIDs, d.ID)
	err := h.deliverErr
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "paseo:" + target.SessionID + ":" + d.ID, nil
}

func (h *workerTestHost) LookupReceipt(_ context.Context, _ HostTarget, _ string) (Receipt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lookupCalls++
	return h.receipt, h.receiptErr
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (h *workerTestHost) counts() (deliveries, lookups int, ids []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deliverCalls, h.lookupCalls, append([]string(nil), h.deliveredIDs...)
}

func newDBWorker(client *dbWorkerClient, host *workerTestHost, workspaceID string) *Worker {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWorker(client, host, func() []string { return []string{workspaceID} }, client.daemonID, logger)
}

func deliveryRowStatus(t *testing.T, pool *pgxpool.Pool, deliveryID string) (status, receipt string) {
	t.Helper()
	var storedReceipt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT delivery_status, host_receipt FROM agent_mailbox_message WHERE id = $1`,
		deliveryID).Scan(&status, &storedReceipt); err != nil {
		t.Fatalf("read delivery %s: %v", deliveryID, err)
	}
	if storedReceipt != nil {
		receipt = *storedReceipt
	}
	return status, receipt
}

func deliveryRowLastError(t *testing.T, pool *pgxpool.Pool, deliveryID string) string {
	t.Helper()
	var reason *string
	if err := pool.QueryRow(context.Background(),
		`SELECT last_error FROM agent_mailbox_message WHERE id = $1`,
		deliveryID).Scan(&reason); err != nil {
		t.Fatalf("read delivery diagnostic %s: %v", deliveryID, err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

func expireLease(t *testing.T, pool *pgxpool.Pool, deliveryID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_mailbox_message SET lease_expires_at = now() - interval '1 second' WHERE id = $1`,
		deliveryID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

func makeClaimableNow(t *testing.T, pool *pgxpool.Pool, deliveryID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_mailbox_message SET next_attempt_at = now() - interval '1 second' WHERE id = $1`,
		deliveryID); err != nil {
		t.Fatalf("schedule delivery: %v", err)
	}
}

// Comment notifications and generic events go through one SQL claim and one
// worker; the host is called once per delivery and queried for no receipts.
func TestWorkerDeliversCommentAndGenericThroughSQLClaim(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	secondSession := addFixtureSession(t, pool, workspaceID, ownerID, agentID, daemonID)
	issueID := addFixtureIssue(t, pool, workspaceID, ownerID)
	comment := enqueueCommentForIssueTo(t, pool, workspaceID, agentID, fixtureSessionID(t, pool, agentID), issueID)
	generic := enqueueGenericTo(t, pool, workspaceID, agentID, secondSession, "run-shared-worker")

	host := &workerTestHost{}
	client := &dbWorkerClient{queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID}
	newDBWorker(client, host, workspaceID).round(context.Background())

	deliveries, lookups, ids := host.counts()
	if deliveries != 2 || lookups != 0 {
		t.Fatalf("host calls = %d deliveries/%d lookups, want 2/0", deliveries, lookups)
	}
	for _, want := range []string{comment.ID, generic.ID} {
		if !containsString(ids, want) {
			t.Fatalf("host did not receive %s: %v", want, ids)
		}
		status, receipt := deliveryRowStatus(t, pool, want)
		if status != "delivered" || receipt == "" {
			t.Fatalf("%s = %s/%q, want delivered with a receipt", want, status, receipt)
		}
	}
}

// A host may accept a request whose acknowledgement never reaches the server.
// After the worker restarts and the lease expires, recovery must look up the
// original receipt: the send count stays flat.
func TestWorkerRecoversAcceptedCommentByReceiptAfterLostAck(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	comment := enqueueCommentForIssue(t, pool, workspaceID, agentID, uuid.NewString())

	firstHost := &workerTestHost{}
	firstClient := &dbWorkerClient{
		queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID, failOutcome: true,
	}
	newDBWorker(firstClient, firstHost, workspaceID).round(context.Background())

	if deliveries, _, _ := firstHost.counts(); deliveries != 1 {
		t.Fatalf("first attempt deliveries = %d, want 1", deliveries)
	}
	if status, _ := deliveryRowStatus(t, pool, comment.ID); status != "leased" {
		t.Fatalf("status after a lost ack = %s, want leased", status)
	}

	// The worker process dies; a fresh worker sees only the persisted row.
	expireLease(t, pool, comment.ID)
	secondHost := &workerTestHost{receipt: Receipt{Status: ReceiptAccepted, Reference: "paseo:accepted:" + comment.ID}}
	secondClient := &dbWorkerClient{queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID}
	newDBWorker(secondClient, secondHost, workspaceID).round(context.Background())

	deliveries, lookups, _ := secondHost.counts()
	if deliveries != 0 || lookups != 1 {
		t.Fatalf("recovery host calls = %d deliveries/%d lookups, want 0/1", deliveries, lookups)
	}
	status, receipt := deliveryRowStatus(t, pool, comment.ID)
	if status != "delivered" || receipt != "paseo:accepted:"+comment.ID {
		t.Fatalf("recovered row = %s/%q, want delivered by receipt", status, receipt)
	}
}

// An absent receipt is not proof of non-execution, and an unsupported lookup is
// not an outcome. Both keep the row unknown with zero new sends.
func TestWorkerKeepsUnknownOnAbsentOrUnsupportedReceipt(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	generic := enqueueGeneric(t, pool, workspaceID, agentID, "run-absent-receipt")

	// Establish an ambiguous row: the host accepted it but the ack was lost.
	firstHost := &workerTestHost{}
	firstClient := &dbWorkerClient{
		queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID, failOutcome: true,
	}
	newDBWorker(firstClient, firstHost, workspaceID).round(context.Background())
	expireLease(t, pool, generic.ID)

	// Absent: the row stays unknown.
	absentHost := &workerTestHost{receipt: Receipt{Status: ReceiptAbsent}}
	absentClient := &dbWorkerClient{queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID}
	newDBWorker(absentClient, absentHost, workspaceID).round(context.Background())
	if deliveries, lookups, _ := absentHost.counts(); deliveries != 0 || lookups != 1 {
		t.Fatalf("absent recovery = %d deliveries/%d lookups, want 0/1", deliveries, lookups)
	}
	if status, _ := deliveryRowStatus(t, pool, generic.ID); status != "unknown" {
		t.Fatalf("absent receipt status = %s, want unknown", status)
	}

	// Unsupported lookup: still unknown, still no send.
	makeClaimableNow(t, pool, generic.ID)
	unsupportedHost := &workerTestHost{receiptErr: &HostError{
		Kind: HostUnsupported, Code: "unsupported_operation", Message: "the host cannot look up receipts",
	}}
	unsupportedClient := &dbWorkerClient{queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID}
	newDBWorker(unsupportedClient, unsupportedHost, workspaceID).round(context.Background())
	if deliveries, lookups, _ := unsupportedHost.counts(); deliveries != 0 || lookups != 1 {
		t.Fatalf("unsupported recovery = %d deliveries/%d lookups, want 0/1", deliveries, lookups)
	}
	if status, _ := deliveryRowStatus(t, pool, generic.ID); status != "unknown" {
		t.Fatalf("unsupported receipt status = %s, want unknown", status)
	}
	if reason := deliveryRowLastError(t, pool, generic.ID); reason == "" {
		t.Fatal("unsupported receipt row lost its diagnostic")
	}

	// The next claim is still verification-only: the server did not turn the
	// absence of a receipt into a delivery attempt.
	makeClaimableNow(t, pool, generic.ID)
	claims, err := NewQueue(db.New(pool), pool).Claim(context.Background(), ClaimRequest{
		Scope: Scope{WorkspaceID: workspaceID}, DaemonID: daemonID, OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim unknown row: %v", err)
	}
	if len(claims) != 1 || claims[0].ID != generic.ID || !claims[0].VerifyOnly {
		t.Fatalf("unknown claim = %+v, want %s verification-only", claims, generic.ID)
	}
}

// A known busy host is a no-effect result: the same delivery is retried with
// its original id and becomes deliverable again once the short delay passes.
func TestWorkerRetriesBusyDeliveryWithTheSameID(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	generic := enqueueGeneric(t, pool, workspaceID, agentID, "run-busy")

	host := &workerTestHost{deliverErr: &HostError{
		Kind: HostBusy, Code: "agent_busy", Message: "the session is busy",
	}}
	client := &dbWorkerClient{queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID}
	worker := newDBWorker(client, host, workspaceID)
	worker.round(context.Background())

	if deliveries, _, _ := host.counts(); deliveries != 1 {
		t.Fatalf("busy deliveries = %d, want 1", deliveries)
	}
	if status, _ := deliveryRowStatus(t, pool, generic.ID); status != "pending" {
		t.Fatalf("busy status = %s, want pending (known no effect)", status)
	}

	makeClaimableNow(t, pool, generic.ID)
	host.mu.Lock()
	host.deliverErr = nil
	host.mu.Unlock()
	worker.round(context.Background())

	deliveries, _, ids := host.counts()
	if deliveries != 2 || ids[0] != generic.ID || ids[1] != generic.ID {
		t.Fatalf("retry = %d deliveries %v, want the same delivery id twice", deliveries, ids)
	}
	if status, _ := deliveryRowStatus(t, pool, generic.ID); status != "delivered" {
		t.Fatalf("retry status = %s, want delivered", status)
	}
}

// A send timeout is an ambiguous outcome, not a terminal failure: the row is
// verified (never re-sent) and stays unknown rather than failed.
func TestWorkerTimeoutStaysUnknownNotFailed(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	generic := enqueueGeneric(t, pool, workspaceID, agentID, "run-timeout")

	host := &workerTestHost{
		deliverErr: &HostError{Kind: HostOutcomeUnknown, Code: "delivery_timeout", Message: "the host send timed out"},
		receipt:    Receipt{Status: ReceiptAbsent},
	}
	client := &dbWorkerClient{queue: NewQueue(db.New(pool), pool), daemonID: daemonID, ownerID: ownerID}
	newDBWorker(client, host, workspaceID).round(context.Background())

	deliveries, lookups, _ := host.counts()
	if deliveries != 1 || lookups != 1 {
		t.Fatalf("timeout calls = %d deliveries/%d lookups, want 1/1 (verify, not resend)", deliveries, lookups)
	}
	if status, _ := deliveryRowStatus(t, pool, generic.ID); status != "unknown" {
		t.Fatalf("timeout status = %s, want unknown instead of a terminal failure", status)
	}
	if reason := deliveryRowLastError(t, pool, generic.ID); reason == "" {
		t.Fatal("timeout row lost its diagnostic")
	}
}

var _ OutcomeClient = (*dbWorkerClient)(nil)
