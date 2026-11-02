package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newClaimFixture creates the minimum rows a claim joins against: workspace,
// owner, external agent and its registered session. The mailbox rows themselves
// have no foreign keys, so a claim test can enqueue against these ids directly.
func newClaimFixture(t *testing.T, pool *pgxpool.Pool) (workspaceID, agentID, ownerID, daemonID string) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()
	ownerID = uuid.NewString()
	workspaceID = uuid.NewString()
	agentID = uuid.NewString()
	daemonID = "daemon-" + suffix

	if _, err := pool.Exec(ctx, `INSERT INTO "user" (id, name, email) VALUES ($1, $2, $3)`,
		ownerID, "delivery owner "+suffix, "delivery-"+suffix+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace (id, name, slug) VALUES ($1, $2, $3)`,
		workspaceID, "delivery "+suffix, "delivery-"+suffix); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		workspaceID, ownerID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, name, runtime_mode, execution_kind, owner_id)
		VALUES ($1, $2, $3, 'cloud', 'external', $4)`,
		agentID, workspaceID, "delivery agent "+suffix, ownerID); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO external_agent_session
			(workspace_id, owner_user_id, agent_id, provider, host_id, session_id, display_name, daemon_id)
		VALUES ($1, $2, $3, 'paseo', $4, $5, $6, $7)`,
		workspaceID, ownerID, agentID, "host-"+suffix, "session-"+suffix, "Delivery Session", daemonID); err != nil {
		t.Fatalf("insert external session: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM agent_mailbox_message WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM external_agent_session WHERE agent_id = $1`, agentID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM "user" WHERE id = $1`, ownerID)
	})
	return workspaceID, agentID, ownerID, daemonID
}

// fixtureSessionID returns the session the fixture registered for one agent.
func fixtureSessionID(t *testing.T, pool *pgxpool.Pool, agentID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM external_agent_session WHERE agent_id = $1 ORDER BY created_at ASC LIMIT 1`,
		util.MustParseUUID(agentID)).Scan(&id); err != nil {
		t.Fatalf("load fixture session: %v", err)
	}
	return id
}

func enqueueGeneric(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, key string) Delivery {
	t.Helper()
	return enqueueGenericTo(t, pool, workspaceID, agentID, fixtureSessionID(t, pool, agentID), key)
}

func enqueueGenericTo(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, targetSessionID, key string) Delivery {
	t.Helper()
	store := NewPostgres(db.New(pool))
	created, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "ci_run", Key: key},
		Target:          Target{SessionID: targetSessionID},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "ci:" + key,
		Kind:            "ci_terminal",
		Payload:         json.RawMessage(`{"run_id":"run-42","conclusion":"success"}`),
	})
	if err != nil {
		t.Fatalf("enqueue generic delivery: %v", err)
	}
	return created
}

func TestQueueClaimSeparatesProjectionsAndBindsToDaemonOwner(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	generic := enqueueGeneric(t, pool, workspaceID, agentID, "run-42")
	comment := enqueueComment(t, pool, workspaceID, agentID)

	queue := NewQueue(db.New(pool), pool)
	genericClaims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Projection:  GenericProjection,
	})
	if err != nil {
		t.Fatalf("claim generic: %v", err)
	}
	if len(genericClaims) != 1 || genericClaims[0].ID != generic.ID {
		t.Fatalf("generic claims = %+v, want only %s", genericClaims, generic.ID)
	}
	if genericClaims[0].Session.SessionID == "" || genericClaims[0].VerifyOnly {
		t.Fatalf("generic claim = %+v", genericClaims[0])
	}
	// One in-flight row per session also blocks the other projection: finish it
	// before asserting the comment entry can lease its own row.
	if _, err := queue.RecordOutcome(context.Background(), OutcomeRequest{
		Scope:      Scope{WorkspaceID: workspaceID},
		DeliveryID: genericClaims[0].ID,
		DaemonID:   daemonID,
		LeaseToken: genericClaims[0].LeaseToken,
		State:      OutcomeDelivered,
	}); err != nil {
		t.Fatalf("record generic delivered: %v", err)
	}

	commentClaims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Projection:  CommentProjection,
	})
	if err != nil {
		t.Fatalf("claim comment: %v", err)
	}
	if len(commentClaims) != 1 || commentClaims[0].ID != comment.ID {
		t.Fatalf("comment claims = %+v, want only %s", commentClaims, comment.ID)
	}

	// A different daemon identity can never lease this recipient's rows.
	otherDaemon := daemonID + "-other"
	if claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    otherDaemon,
		OwnerUserID: ownerID,
		Projection:  GenericProjection,
	}); err != nil || len(claims) != 0 {
		t.Fatalf("foreign daemon claims = %+v err=%v, want none", claims, err)
	}
}

func enqueueComment(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID string) Delivery {
	t.Helper()
	return enqueueCommentForIssue(t, pool, workspaceID, agentID, uuid.NewString())
}

func enqueueCommentForIssue(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, issueID string) Delivery {
	t.Helper()
	commentID := uuid.NewString()
	store := NewPostgres(db.New(pool))
	created, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "comment", Key: commentID},
		Target:          Target{SessionID: fixtureSessionID(t, pool, agentID)},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "comment:" + commentID + ":agent:" + agentID,
		Kind:            "comment_mention",
		Payload:         json.RawMessage(fmt.Sprintf(`{"schema":"comment_mention.v1","issue_id":%q,"comment_id":%q}`, issueID, commentID)),
		Refs:            Refs{IssueID: issueID, CommentID: commentID},
	})
	if err != nil {
		t.Fatalf("enqueue comment delivery: %v", err)
	}
	return created
}

func TestQueueClaimLeasesOneRowPerSessionUntilAcknowledged(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	enqueueGeneric(t, pool, workspaceID, agentID, "run-1")
	enqueueGeneric(t, pool, workspaceID, agentID, "run-2")

	queue := NewQueue(db.New(pool), pool)
	request := ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Projection:  GenericProjection,
	}

	claims, err := queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("first claim = %d rows, want one per session", len(claims))
	}
	claimsAgain, err := queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(claimsAgain) != 0 {
		t.Fatalf("second claim = %+v, want no overlap with the in-flight lease", claimsAgain)
	}

	deliveredID := claims[0].ID
	if _, err := queue.RecordOutcome(context.Background(), OutcomeRequest{
		Scope:      Scope{WorkspaceID: workspaceID},
		DeliveryID: deliveredID,
		DaemonID:   daemonID,
		LeaseToken: claims[0].LeaseToken,
		State:      OutcomeDelivered,
	}); err != nil {
		t.Fatalf("record delivered: %v", err)
	}
	// The delivered row no longer blocks the session's next request.
	claims, err = queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("claim after ack: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claim after ack = %+v, want the other delivery", claims)
	}
	if claims[0].ID == deliveredID {
		t.Fatalf("claim after ack returned the completed delivery %s", deliveredID)
	}
}

func TestQueueRecordOutcomeBusyShortDelayAndStaleTokenFences(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	created := enqueueGeneric(t, pool, workspaceID, agentID, "run-42")

	queue := NewQueue(db.New(pool), pool)
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Projection:  GenericProjection,
	})
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v err=%v", claims, err)
	}
	request := OutcomeRequest{
		Scope:      Scope{WorkspaceID: workspaceID},
		DeliveryID: claims[0].ID,
		DaemonID:   daemonID,
		LeaseToken: claims[0].LeaseToken,
		State:      OutcomeRetry,
		RetryClass: RetryBusy,
		Error:      "agent_busy",
	}
	if _, err := queue.RecordOutcome(context.Background(), request); err != nil {
		t.Fatalf("record busy: %v", err)
	}
	var status string
	var nextAttempt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT delivery_status, next_attempt_at FROM agent_mailbox_message WHERE id = $1`,
		created.ID).Scan(&status, &nextAttempt); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if delay := time.Until(nextAttempt); delay > 30*time.Second {
		t.Fatalf("busy retry delay = %v, want a short delay rather than the failure backoff", delay)
	}

	// The same token cannot acknowledge the row twice.
	if _, err := queue.RecordOutcome(context.Background(), request); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("second ack error = %v, want ErrLeaseLost", err)
	}
}

func TestQueueReclaimsExpiredLeaseAndUnknownForVerificationOnly(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	expired := enqueueGeneric(t, pool, workspaceID, agentID, "run-expired")

	queue := NewQueue(db.New(pool), pool)
	request := ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Projection:  GenericProjection,
	}
	claims, err := queue.Claim(context.Background(), request)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v err=%v", claims, err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_mailbox_message SET lease_expires_at = now() - interval '1 second' WHERE id = $1`,
		expired.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	reclaimed, err := queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 || !reclaimed[0].VerifyOnly {
		t.Fatalf("reclaimed = %+v, want verification-only", reclaimed)
	}

	if _, err := queue.RecordOutcome(context.Background(), OutcomeRequest{
		Scope:      Scope{WorkspaceID: workspaceID},
		DeliveryID: reclaimed[0].ID,
		DaemonID:   daemonID,
		LeaseToken: reclaimed[0].LeaseToken,
		State:      OutcomeUnknown,
		Error:      "the host has no receipt; absent is not proof of non-execution",
	}); err != nil {
		t.Fatalf("record unknown: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_mailbox_message SET next_attempt_at = now() - interval '1 second' WHERE id = $1`,
		expired.ID); err != nil {
		t.Fatalf("schedule verification: %v", err)
	}
	verify, err := queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("verify claim: %v", err)
	}
	if len(verify) != 1 || !verify[0].VerifyOnly {
		t.Fatalf("verify claim = %+v, want a verification-only row", verify)
	}
	if !strings.Contains(verify[0].LastError, "absent is not proof") {
		t.Fatalf("verify claim lost the unconfirmed diagnostic: %q", verify[0].LastError)
	}
}

func TestQueueClaimSkipsArchivedOrReboundSessions(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	enqueueGeneric(t, pool, workspaceID, agentID, "run-42")

	queue := NewQueue(db.New(pool), pool)
	request := ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Projection:  GenericProjection,
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent SET archived_at = now() WHERE id = $1`, agentID); err != nil {
		t.Fatalf("archive agent: %v", err)
	}
	claims, err := queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("claim archived: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("archived agent claims = %+v, want none", claims)
	}
}

// TestLeaseClaimableDeliveryRechecksInflightAfterLock reproduces the
// interleaving where the comment entry and the generic entry each list a
// candidate for the same recipient before either leases one. The generic list
// is a pre-lock snapshot, but its lease statement runs after the comment claim
// committed. The lease itself must re-check the recipient's in-flight rows;
// otherwise both projections rent the same session even though the advisory
// lock only serialised the two claims.
func TestLeaseClaimableDeliveryRechecksInflightAfterLock(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	generic := enqueueGeneric(t, pool, workspaceID, agentID, "run-race")
	comment := enqueueComment(t, pool, workspaceID, agentID)

	ctx := context.Background()
	queries := db.New(pool)
	workspaceUUID := mustParseUUID(t, workspaceID)
	recipientUUID := mustParseUUID(t, agentID)
	ownerUUID := mustParseUUID(t, ownerID)
	targetSessionID := fixtureSessionID(t, pool, agentID)
	targetSessionUUID := mustParseUUID(t, targetSessionID)

	list := func(tx pgx.Tx, commentProjection bool) []db.ListClaimableDeliveryIDsRow {
		t.Helper()
		rows, err := queries.WithTx(tx).ListClaimableDeliveryIDs(ctx, db.ListClaimableDeliveryIDsParams{
			WorkspaceID:       workspaceUUID,
			CommentProjection: commentProjection,
			OwnerUserID:       ownerUUID,
			DaemonID:          daemonID,
			RowLimit:          5,
		})
		if err != nil {
			t.Fatalf("list claimable (comment=%v): %v", commentProjection, err)
		}
		return rows
	}
	lease := func(tx pgx.Tx, deliveryID, token string) error {
		t.Helper()
		_, err := queries.WithTx(tx).LeaseClaimableDelivery(ctx, db.LeaseClaimableDeliveryParams{
			ID:              mustParseUUID(t, deliveryID),
			WorkspaceID:     workspaceUUID,
			TargetSessionID: targetSessionUUID,
			LeaseTokenHash:  pgtype.Text{String: token, Valid: true},
			LeaseOwner:      pgtype.Text{String: daemonID, Valid: true},
			LeaseExpiresAt:  pgtypeTime(time.Now().Add(time.Minute)),
		})
		return err
	}

	// The generic entry lists first, before anyone holds a lease, so its
	// snapshot shows no in-flight row for this recipient.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin generic claim: %v", err)
	}
	defer txB.Rollback(ctx)
	genericCandidates := list(txB, false)
	if len(genericCandidates) != 1 || util.UUIDToString(genericCandidates[0].ID) != generic.ID {
		t.Fatalf("generic candidates = %+v, want only %s", genericCandidates, generic.ID)
	}

	// The comment entry lists, locks the recipient, leases its row and commits
	// while the generic claim transaction is still open.
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin comment claim: %v", err)
	}
	commentCandidates := list(txA, true)
	if len(commentCandidates) != 1 || util.UUIDToString(commentCandidates[0].ID) != comment.ID {
		txA.Rollback(ctx)
		t.Fatalf("comment candidates = %+v, want only %s", commentCandidates, comment.ID)
	}
	locked, err := queries.WithTx(txA).TryLockDeliveryTarget(ctx, targetSessionID)
	if err != nil {
		txA.Rollback(ctx)
		t.Fatalf("lock recipient for comment claim: %v", err)
	}
	if !locked {
		txA.Rollback(ctx)
		t.Fatal("comment claim could not take the recipient lock")
	}
	if err := lease(txA, comment.ID, "comment-lease"); err != nil {
		txA.Rollback(ctx)
		t.Fatalf("lease comment row: %v", err)
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit comment claim: %v", err)
	}

	// The generic claimant now takes the released lock and leases its stale
	// candidate. The row is still eligible on its own, but the recipient already
	// has a committed in-flight lease, so the lease must match no row.
	locked, err = queries.WithTx(txB).TryLockDeliveryTarget(ctx, targetSessionID)
	if err != nil {
		t.Fatalf("lock recipient for generic claim: %v", err)
	}
	if !locked {
		t.Fatal("generic claim could not take the recipient lock after the comment claim committed")
	}
	if err := lease(txB, generic.ID, "generic-lease"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("generic lease error = %v, want pgx.ErrNoRows", err)
	}
	if err := txB.Rollback(ctx); err != nil {
		t.Fatalf("rollback generic claim: %v", err)
	}

	var inflight int
	var leasedID string
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(min(id::text), '')
		FROM agent_mailbox_message
		WHERE recipient_agent_id = $1
		  AND delivery_status = 'leased'
		  AND lease_expires_at > now()`, recipientUUID).Scan(&inflight, &leasedID); err != nil {
		t.Fatalf("count in-flight rows: %v", err)
	}
	if inflight != 1 || leasedID != comment.ID {
		t.Fatalf("in-flight rows = %d (lease %s), want only the comment row %s", inflight, leasedID, comment.ID)
	}
}

// A generic delivery may carry an issue reference, but the frozen issue
// projection only renders comment-shaped rows. It must never leak a generic row
// into the issue response.
func TestListIssueMailboxDeliveriesExcludesGenericRows(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, _, _ := newClaimFixture(t, pool)
	issueID := uuid.NewString()
	comment := enqueueCommentForIssue(t, pool, workspaceID, agentID, issueID)

	store := NewPostgres(db.New(pool))
	generic, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "ci_run", Key: "run-issue-" + uuid.NewString()},
		Target:          Target{SessionID: fixtureSessionID(t, pool, agentID)},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "ci:run-issue:" + uuid.NewString(),
		Kind:            "ci_terminal",
		Refs:            Refs{IssueID: issueID},
		Payload:         json.RawMessage(`{"run_id":"run-42","conclusion":"success"}`),
	})
	if err != nil {
		t.Fatalf("enqueue generic delivery with an issue reference: %v", err)
	}

	rows, err := db.New(pool).ListIssueMailboxDeliveries(context.Background(), db.ListIssueMailboxDeliveriesParams{
		IssueID:     mustParseUUID(t, issueID),
		WorkspaceID: mustParseUUID(t, workspaceID),
	})
	if err != nil {
		t.Fatalf("list issue mailbox deliveries: %v", err)
	}
	if len(rows) != 1 || util.UUIDToString(rows[0].ID) != comment.ID {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, util.UUIDToString(row.ID))
		}
		t.Fatalf("issue deliveries = %v, want only the comment row %s (generic %s must not leak)", ids, comment.ID, generic.ID)
	}
}

func mustParseUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	parsed, err := util.ParseUUID(value)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", value, err)
	}
	return parsed
}
