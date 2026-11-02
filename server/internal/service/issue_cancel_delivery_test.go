package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// mailboxState is the part of one notification a cancellation test judges.
type mailboxState struct {
	Status        string
	LastError     pgtype.Text
	LeaseToken    pgtype.Text
	NextAttemptAt pgtype.Timestamptz
	WithdrawnAt   pgtype.Timestamptz
}

func readMailboxState(t *testing.T, f principalFixture, deliveryID string) mailboxState {
	t.Helper()
	var state mailboxState
	if err := f.Pool.QueryRow(context.Background(), `
		SELECT delivery_status, last_error, lease_token_hash, next_attempt_at, withdrawn_at
		FROM agent_mailbox_message WHERE id = $1`, deliveryID).Scan(
		&state.Status, &state.LastError, &state.LeaseToken, &state.NextAttemptAt, &state.WithdrawnAt); err != nil {
		t.Fatalf("read mailbox %s: %v", deliveryID, err)
	}
	return state
}

// enqueueCommentNotification stores a comment mention for the fixture's
// external session, the same durable row the comment dispatch produces.
func enqueueCommentNotification(t *testing.T, f principalFixture, issue pgtype.UUID, agent, label string) delivery.Delivery {
	t.Helper()
	commentID := uuid.NewString()
	created, err := delivery.NewPostgres(f.q).Enqueue(context.Background(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: f.WorkspaceID},
		Source:          delivery.Source{Type: "comment", Key: commentID},
		Target:          delivery.Target{SessionID: fixtureExternalSessionID(t, f, agent)},
		Recipient:       delivery.Recipient{Type: "agent", ID: agent},
		NotificationKey: fmt.Sprintf("cancel-test:comment:%s:%s", label, commentID),
		Kind:            "comment_mention",
		Payload:         json.RawMessage(fmt.Sprintf(`{"schema":"comment_mention.v1","issue_id":%q,"comment_id":%q}`, util.UUIDToString(issue), commentID)),
		Refs:            delivery.Refs{IssueID: util.UUIDToString(issue), CommentID: commentID},
	})
	if err != nil {
		t.Fatalf("enqueue comment notification: %v", err)
	}
	return created
}

// enqueueWakeupNotification stores a generic issue-wakeup notification, which
// is the second source type the cancellation must cover.
func enqueueWakeupNotification(t *testing.T, f principalFixture, issue pgtype.UUID, agent, label string) delivery.Delivery {
	t.Helper()
	created, err := delivery.NewPostgres(f.q).Enqueue(context.Background(), delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: f.WorkspaceID},
		Source:          delivery.Source{Type: "issue_wakeup", Key: "cancel-test:" + label},
		Target:          delivery.Target{SessionID: fixtureExternalSessionID(t, f, agent)},
		Recipient:       delivery.Recipient{Type: "agent", ID: agent},
		NotificationKey: "cancel-test:wakeup:" + label,
		Kind:            "issue_wakeup",
		Payload:         json.RawMessage(`{"schema":"issue_wakeup.v1","instruction":"Check the job"}`),
		Refs:            delivery.Refs{IssueID: util.UUIDToString(issue)},
	})
	if err != nil {
		t.Fatalf("enqueue wakeup notification: %v", err)
	}
	return created
}

func claimIssueNotifications(t *testing.T, f principalFixture, daemonID string) []delivery.Claim {
	t.Helper()
	claims, err := delivery.NewQueue(f.q, f.Pool).Claim(context.Background(), delivery.ClaimRequest{
		Scope:       delivery.Scope{WorkspaceID: f.WorkspaceID},
		DaemonID:    daemonID,
		OwnerUserID: f.UserID,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("claim notifications: %v", err)
	}
	return claims
}

// Cancelling an issue withdraws every notification no host has accepted yet,
// whatever produced it, and a later explicit request is a new notification
// rather than a resurrection of the withdrawn ones.
func TestIssueCancelWithdrawsUnclaimedNotificationsForEverySource(t *testing.T) {
	f, _, issue, agent, daemonID := externalWakeFixture(t)
	comment := enqueueCommentNotification(t, f, issue, agent, "before")
	generic := enqueueWakeupNotification(t, f, issue, agent, "before")
	// A legacy internal-mailbox row has no target session and is consumed
	// through processing_status; the delivery-core withdrawal must not rewrite
	// its delivery status into a failure nothing can clear.
	legacyID := uuid.NewString()
	f.Exec(t, `INSERT INTO agent_mailbox_message
			(id, workspace_id, issue_id, recipient_agent_id, recipient_type, source_type, source_key, kind, payload, sender_type, sender_id)
		VALUES ($1,$2,$3,$4,'agent','comment',$5,'comment_mention','{}'::jsonb,'member',$6)`,
		legacyID, f.WorkspaceID, util.UUIDToString(issue), agent, "legacy:"+legacyID, f.UserID)

	wakeSetStatus(t, f, issue, "cancelled")

	for name, created := range map[string]delivery.Delivery{"comment": comment, "generic": generic} {
		state := readMailboxState(t, f, created.ID)
		if state.Status != "failed" {
			t.Fatalf("%s notification after cancellation = %q, want failed", name, state.Status)
		}
		if !state.LastError.Valid || state.LastError.String == "" {
			t.Fatalf("%s notification after cancellation carries no reason", name)
		}
		if !state.WithdrawnAt.Valid {
			t.Fatalf("%s notification after cancellation carries no withdrawal marker", name)
		}
	}
	if claims := claimIssueNotifications(t, f, daemonID); len(claims) != 0 {
		t.Fatalf("cancellation left %d send-authorizing claims: %+v", len(claims), claims)
	}
	if legacy := readMailboxState(t, f, legacyID); legacy.Status != "pending" || legacy.WithdrawnAt.Valid {
		t.Fatalf("the legacy internal-mailbox row was rewritten: status %q withdrawn %+v", legacy.Status, legacy.WithdrawnAt)
	}

	// A new explicit mention is a new request. It must still be deliverable on
	// a cancelled issue; cancellation withdraws, it does not ban the issue.
	after := enqueueCommentNotification(t, f, issue, agent, "after")
	claims := claimIssueNotifications(t, f, daemonID)
	if len(claims) != 1 || claims[0].ID != after.ID {
		t.Fatalf("new request after cancellation = %+v, want the new notification", claims)
	}
	if claims[0].VerifyOnly {
		t.Fatalf("new request after cancellation is verification-only: %+v", claims[0])
	}

	// Reopening the issue must not make the withdrawn notification claimable
	// again; only the notification created after the cancellation exists.
	wakeSetStatus(t, f, issue, "todo")
	if _, err := delivery.NewQueue(f.q, f.Pool).RecordOutcome(context.Background(), delivery.OutcomeRequest{
		Scope:       delivery.Scope{WorkspaceID: f.WorkspaceID},
		DeliveryID:  claims[0].ID,
		DaemonID:    daemonID,
		LeaseToken:  claims[0].LeaseToken,
		State:       delivery.OutcomeDelivered,
		HostReceipt: "receipt",
	}); err != nil {
		t.Fatalf("record the new notification: %v", err)
	}
	if reopened := claimIssueNotifications(t, f, daemonID); len(reopened) != 0 {
		t.Fatalf("reopening resurrected withdrawn notifications: %+v", reopened)
	}
}

// A cancellation that lands while the host call is in flight must remember
// the withdrawal: a known no-effect answer cannot put the notification back
// into claimable delivery and send it again.
func TestIssueCancelAfterClaimThenBusyCannotResurrect(t *testing.T) {
	f, _, issue, agent, daemonID := externalWakeFixture(t)
	created := enqueueCommentNotification(t, f, issue, agent, "busy")

	entered := make(chan struct{})
	release := make(chan error, 1)
	host := &countingHost{onSend: func(_ context.Context, attempt int) error {
		if attempt == 1 {
			close(entered)
			return <-release
		}
		return nil
	}}
	worker := newWakeupTestWorker(f, daemonID, host)
	cancel, done := startWakeupTestWorker(worker)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		stopWakeupTestWorker(t, cancel, done)
		t.Fatal("the worker never attempted the notification")
	}

	wakeSetStatus(t, f, issue, "cancelled")
	release <- &delivery.HostError{Kind: delivery.HostBusy, Code: "agent_busy"}
	waitForWakeup(t, "the cancelled notification to settle", func() bool {
		return readMailboxState(t, f, created.ID).Status == "failed"
	})
	stopWakeupTestWorker(t, cancel, done)

	state := readMailboxState(t, f, created.ID)
	if state.LeaseToken.Valid {
		t.Fatalf("cancelled notification kept a lease: %+v", state)
	}
	if !state.LastError.Valid || !strings.Contains(state.LastError.String, "closed") {
		t.Fatalf("cancelled notification reason = %+v, want the withdrawal reason", state.LastError)
	}
	if !state.WithdrawnAt.Valid {
		t.Fatal("cancelled notification carries no withdrawal marker")
	}
	// Give the row another delivery opportunity; the withdrawal still wins.
	f.Exec(t, "UPDATE agent_mailbox_message SET next_attempt_at=now() WHERE id=$1", created.ID)
	runWakeupTestWorkerRound(t, newWakeupTestWorker(f, daemonID, host))
	if sends, lookups := host.counts(); sends != 1 || lookups != 0 {
		t.Fatalf("after the cancellation the host saw %d sends and %d lookups, want 1 send and no replay", sends, lookups)
	}
}

// An accepted answer after the cancellation is the truth: the notification was
// delivered, and the withdrawal marker cannot rewrite that.
func TestIssueCancelAfterClaimThenAcceptedStaysDelivered(t *testing.T) {
	f, _, issue, agent, daemonID := externalWakeFixture(t)
	created := enqueueCommentNotification(t, f, issue, agent, "accepted")

	entered := make(chan struct{})
	release := make(chan error, 1)
	host := &countingHost{onSend: func(_ context.Context, attempt int) error {
		if attempt == 1 {
			close(entered)
			return <-release
		}
		return nil
	}}
	worker := newWakeupTestWorker(f, daemonID, host)
	cancel, done := startWakeupTestWorker(worker)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		stopWakeupTestWorker(t, cancel, done)
		t.Fatal("the worker never attempted the notification")
	}
	wakeSetStatus(t, f, issue, "cancelled")
	release <- nil
	waitForWakeup(t, "the accepted notification", func() bool {
		return readMailboxState(t, f, created.ID).Status == "delivered"
	})
	stopWakeupTestWorker(t, cancel, done)

	if sends, _ := host.counts(); sends != 1 {
		t.Fatalf("host sends = %d, want 1", sends)
	}
}

// An unknown outcome after the cancellation keeps the original delivery id and
// is only ever looked up by receipt; it is never sent a second time.
func TestIssueCancelAfterClaimThenUnknownKeepsReceiptLookup(t *testing.T) {
	f, _, issue, agent, daemonID := externalWakeFixture(t)
	created := enqueueCommentNotification(t, f, issue, agent, "unknown")

	entered := make(chan struct{})
	release := make(chan error, 1)
	host := &countingHost{onSend: func(_ context.Context, attempt int) error {
		if attempt == 1 {
			close(entered)
			return <-release
		}
		return nil
	}}
	worker := newWakeupTestWorker(f, daemonID, host)
	cancel, done := startWakeupTestWorker(worker)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		stopWakeupTestWorker(t, cancel, done)
		t.Fatal("the worker never attempted the notification")
	}
	wakeSetStatus(t, f, issue, "cancelled")
	release <- &delivery.HostError{Kind: delivery.HostOutcomeUnknown, Code: "ack_lost"}
	waitForWakeup(t, "the unknown outcome", func() bool {
		return readMailboxState(t, f, created.ID).Status == "unknown"
	})
	stopWakeupTestWorker(t, cancel, done)

	if sends, lookups := host.counts(); sends != 1 || lookups != 1 {
		t.Fatalf("host saw %d sends and %d lookups, want one send and one receipt lookup", sends, lookups)
	}
	// The next claim is a receipt check of the same delivery, not a new send.
	f.Exec(t, "UPDATE agent_mailbox_message SET next_attempt_at=now() WHERE id=$1", created.ID)
	claims := claimIssueNotifications(t, f, daemonID)
	if len(claims) != 1 || claims[0].ID != created.ID || !claims[0].VerifyOnly {
		t.Fatalf("reclaim after unknown = %+v, want the same id verification-only", claims)
	}
}

// The status write and the withdrawal share one transaction: when the
// withdrawal cannot run, the cancellation must roll back instead of leaving a
// cancelled issue whose old notification is still claimable.
func TestIssueCancelRollsBackWhenWithdrawalCannotRun(t *testing.T) {
	f, _, issue, agent, daemonID := externalWakeFixture(t)
	created := enqueueCommentNotification(t, f, issue, agent, "rollback")
	ctx := context.Background()

	// Hold the notification row so the withdrawal blocks in the cancel
	// transaction; the transaction's lock timeout turns that into an error.
	holder, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "SELECT 1 FROM agent_mailbox_message WHERE id=$1 FOR UPDATE", created.ID); err != nil {
		holder.Rollback(ctx)
		t.Fatal(err)
	}

	cancelTx, err := f.Pool.Begin(ctx)
	if err != nil {
		holder.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := cancelTx.Exec(ctx, "SET LOCAL lock_timeout='150ms'"); err != nil {
		cancelTx.Rollback(ctx)
		holder.Rollback(ctx)
		t.Fatal(err)
	}
	q := f.q.WithTx(cancelTx)
	updated, err := q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID: issue, Status: "cancelled", WorkspaceID: parseTestUUID(t, f.WorkspaceID),
	})
	if err != nil {
		cancelTx.Rollback(ctx)
		holder.Rollback(ctx)
		t.Fatalf("status update: %v", err)
	}
	if _, err := StopClosedIssueWakeups(ctx, q, updated); err == nil {
		cancelTx.Rollback(ctx)
		holder.Rollback(ctx)
		t.Fatal("the withdrawal blocked but the cancel transaction reported success")
	}
	if err := cancelTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var status string
	if err := f.Pool.QueryRow(ctx, "SELECT status FROM issue WHERE id=$1", issue).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "cancelled" {
		t.Fatal("the failed withdrawal still committed the cancellation")
	}
	if state := readMailboxState(t, f, created.ID); state.WithdrawnAt.Valid {
		t.Fatalf("the rolled-back transaction left a withdrawal marker: %+v", state.WithdrawnAt)
	}
	claims := claimIssueNotifications(t, f, daemonID)
	if len(claims) != 1 || claims[0].ID != created.ID || claims[0].VerifyOnly {
		t.Fatalf("after the rolled-back cancellation claims = %+v, want the untouched notification", claims)
	}
}
