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

// Every source type shares one claim. A comment notification and a generic
// event are leased by the same call, each bound to the verified daemon/owner.
func TestQueueClaimCoversCommentAndGenericAndBindsToDaemonOwner(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	secondSession := addFixtureSession(t, pool, workspaceID, ownerID, agentID, daemonID)
	generic := enqueueGenericTo(t, pool, workspaceID, agentID, secondSession, "run-42")
	comment := enqueueComment(t, pool, workspaceID, agentID)

	queue := NewQueue(db.New(pool), pool)
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want the comment and the generic row", claims)
	}
	byID := map[string]Claim{}
	for _, claim := range claims {
		byID[claim.ID] = claim
	}
	if _, ok := byID[comment.ID]; !ok {
		t.Fatalf("comments were not claimable through the unified path: %+v", claims)
	}
	if _, ok := byID[generic.ID]; !ok {
		t.Fatalf("generic deliveries were not claimable through the unified path: %+v", claims)
	}
	for _, claim := range claims {
		if claim.Session.SessionID == "" || claim.VerifyOnly {
			t.Fatalf("first claim of %s = %+v, want a deliverable lease", claim.ID, claim)
		}
	}

	// A different daemon identity can never lease this recipient's rows.
	otherDaemon := daemonID + "-other"
	if claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    otherDaemon,
		OwnerUserID: ownerID,
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
	if _, err := pool.Exec(context.Background(), `UPDATE agent_mailbox_message SET attempts = 100 WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}

	queue := NewQueue(db.New(pool), pool)
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
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
	stored, err := queue.RecordOutcome(context.Background(), request)
	if err != nil {
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
	if stored.NextAttemptAt == nil || !stored.NextAttemptAt.Equal(nextAttempt) {
		t.Fatalf("returned next attempt = %v, database = %v", stored.NextAttemptAt, nextAttempt)
	}

	// The same token cannot acknowledge the row twice.
	if _, err := queue.RecordOutcome(context.Background(), request); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("second ack error = %v, want ErrLeaseLost", err)
	}
}

func TestQueueUnavailableRetryBackoff(t *testing.T) {
	for _, tc := range []struct {
		streak int
		delay  time.Duration
	}{
		{0, 15 * time.Second}, {1, 60 * time.Second}, {3, 120 * time.Second},
	} {
		t.Run(fmt.Sprint(tc.streak), func(t *testing.T) {
			pool := openDeliveryTestPool(t)
			workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
			created := enqueueGeneric(t, pool, workspaceID, agentID, "offline")
			if _, err := pool.Exec(context.Background(), `UPDATE agent_mailbox_message SET attempts = 100, delivery_failure_streak = $2 WHERE id = $1`, created.ID, tc.streak); err != nil {
				t.Fatal(err)
			}
			queue := NewQueue(db.New(pool), pool)
			claims, err := queue.Claim(context.Background(), ClaimRequest{Scope: Scope{WorkspaceID: workspaceID}, DaemonID: daemonID, OwnerUserID: ownerID})
			if err != nil || len(claims) != 1 {
				t.Fatalf("claim = %+v, err = %v", claims, err)
			}
			before := time.Now()
			stored, err := queue.RecordOutcome(context.Background(), OutcomeRequest{Scope: Scope{WorkspaceID: workspaceID}, DeliveryID: created.ID, DaemonID: daemonID, LeaseToken: claims[0].LeaseToken, State: OutcomeRetry, RetryClass: RetryUnavailable})
			if err != nil || stored.NextAttemptAt == nil {
				t.Fatalf("record = %+v, err = %v", stored, err)
			}
			delay := stored.NextAttemptAt.Sub(before)
			if delay < tc.delay-time.Second || delay > tc.delay+time.Second {
				t.Fatalf("delay = %v, want %v", delay, tc.delay)
			}
		})
	}
}

func TestBusyResetsFailureStreakBeforeOffline(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	created := enqueueGeneric(t, pool, workspaceID, agentID, "busy-then-offline")
	queue := NewQueue(db.New(pool), pool)
	request := ClaimRequest{Scope: Scope{WorkspaceID: workspaceID}, DaemonID: daemonID, OwnerUserID: ownerID}
	for i := 0; i < 12; i++ {
		claims, err := queue.Claim(context.Background(), request)
		if err != nil || len(claims) != 1 {
			t.Fatalf("busy claim = %v, %v", claims, err)
		}
		_, err = queue.RecordOutcome(context.Background(), OutcomeRequest{Scope: request.Scope, DeliveryID: created.ID, DaemonID: daemonID, LeaseToken: claims[0].LeaseToken, State: OutcomeRetry, RetryClass: RetryBusy})
		if err != nil {
			t.Fatal(err)
		}
		makeClaimableNow(t, pool, created.ID)
	}
	claims, err := queue.Claim(context.Background(), request)
	if err != nil || len(claims) != 1 {
		t.Fatalf("offline claim = %v, %v", claims, err)
	}
	before := time.Now()
	stored, err := queue.RecordOutcome(context.Background(), OutcomeRequest{Scope: request.Scope, DeliveryID: created.ID, DaemonID: daemonID, LeaseToken: claims[0].LeaseToken, State: OutcomeRetry, RetryClass: RetryUnavailable})
	if err != nil || stored.NextAttemptAt == nil {
		t.Fatalf("outcome = %v, %v", stored, err)
	}
	if delay := stored.NextAttemptAt.Sub(before); delay < 14*time.Second || delay > 16*time.Second {
		t.Fatalf("delay = %v, want 15s", delay)
	}
}

func TestHostRecoveryWakesOnlyOfflinePending(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	session := fixtureSessionID(t, pool, agentID)
	recovered := enqueueGeneric(t, pool, workspaceID, agentID, "recovered")
	offline := enqueueGeneric(t, pool, workspaceID, agentID, "offline-wake")
	unknown := enqueueGeneric(t, pool, workspaceID, agentID, "unknown-keep")
	dependency := enqueueGeneric(t, pool, workspaceID, agentID, "dependency-keep")
	other := enqueueGeneric(t, pool, workspaceID, agentID, "other-host")
	var otherSession string
	if err := pool.QueryRow(context.Background(), `INSERT INTO external_agent_session (workspace_id, owner_user_id, agent_id, provider, host_id, session_id, display_name, daemon_id) VALUES ($1,$2,$3,'paseo','other-host',$4,'other',$5) RETURNING id::text`, workspaceID, ownerID, agentID, uuid.NewString(), daemonID).Scan(&otherSession); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(context.Background(), `UPDATE agent_mailbox_message SET target_session_id = CASE WHEN id = $5 THEN $6::uuid ELSE target_session_id END, delivery_status = CASE WHEN id = $3 THEN 'unknown' ELSE 'pending' END, delivery_retry_class = CASE WHEN id = $4 THEN 'dependency' ELSE 'unavailable' END, next_attempt_at = now() + interval '10 minutes' WHERE id IN ($1,$2,$3,$4,$5)`, recovered.ID, offline.ID, unknown.ID, dependency.ID, other.ID, otherSession)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE agent_mailbox_message SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, recovered.ID); err != nil {
		t.Fatal(err)
	}
	queue := NewQueue(db.New(pool), pool)
	request := ClaimRequest{Scope: Scope{WorkspaceID: workspaceID}, DaemonID: daemonID, OwnerUserID: ownerID}
	claims, err := queue.Claim(context.Background(), request)
	if err != nil || len(claims) != 1 || claims[0].Session.SessionID == "" || claims[0].Target.SessionID != session {
		t.Fatalf("claims = %v, err = %v", claims, err)
	}
	_, err = queue.RecordOutcome(context.Background(), OutcomeRequest{Scope: request.Scope, DeliveryID: recovered.ID, DaemonID: daemonID, LeaseToken: claims[0].LeaseToken, State: OutcomeRetry, RetryClass: RetryBusy})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		ready bool
	}{{offline.ID, true}, {unknown.ID, false}, {dependency.ID, false}, {other.ID, false}} {
		var ready bool
		if err := pool.QueryRow(context.Background(), `SELECT next_attempt_at <= now() FROM agent_mailbox_message WHERE id = $1`, tc.id).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready != tc.ready {
			t.Errorf("delivery %s ready = %v, want %v", tc.id, ready, tc.ready)
		}
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
// TestLeaseClaimableDeliveryRechecksInflightAfterLock reproduces the
// interleaving where two claim rounds list the same session's older row before
// either leases it. The second list is a pre-lock snapshot, but its lease
// statement runs after the first claim committed. The lease itself must
// re-check the session's in-flight rows; otherwise a stale candidate — or a
// different row of the same session — is leased twice.
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

	list := func(tx pgx.Tx) []db.ListClaimableDeliveryIDsRow {
		t.Helper()
		rows, err := queries.WithTx(tx).ListClaimableDeliveryIDs(ctx, db.ListClaimableDeliveryIDsParams{
			WorkspaceID: workspaceUUID,
			OwnerUserID: ownerUUID,
			DaemonID:    daemonID,
			RowLimit:    5,
		})
		if err != nil {
			t.Fatalf("list claimable: %v", err)
		}
		return rows
	}
	lease := func(tx pgx.Tx, deliveryID, token string) error {
		t.Helper()
		_, err := queries.WithTx(tx).LeaseClaimableDelivery(ctx, db.LeaseClaimableDeliveryParams{
			ID:              mustParseUUID(t, deliveryID),
			WorkspaceID:     workspaceUUID,
			TargetSessionID: targetSessionUUID,
			OwnerUserID:     ownerUUID,
			DaemonID:        daemonID,
			LeaseTokenHash:  pgtype.Text{String: token, Valid: true},
			LeaseOwner:      pgtype.Text{String: daemonID, Valid: true},
			LeaseExpiresAt:  pgtypeTime(time.Now().Add(time.Minute)),
		})
		return err
	}

	// Both claim transactions list before anyone holds a lease. The session is
	// the same, so each snapshot sees only its oldest row.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second claim: %v", err)
	}
	defer txB.Rollback(ctx)
	secondCandidates := list(txB)
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first claim: %v", err)
	}
	firstCandidates := list(txA)
	if len(firstCandidates) != 1 || len(secondCandidates) != 1 || firstCandidates[0].ID != secondCandidates[0].ID {
		txA.Rollback(ctx)
		t.Fatalf("candidates = %+v and %+v, want the same single row", firstCandidates, secondCandidates)
	}
	olderID := util.UUIDToString(firstCandidates[0].ID)
	inflight := comment
	if olderID == generic.ID {
		inflight = generic
	}

	// The first claimant locks the session, leases its row and commits while
	// the second claim transaction is still open.
	locked, err := queries.WithTx(txA).TryLockDeliveryTarget(ctx, targetSessionID)
	if err != nil {
		txA.Rollback(ctx)
		t.Fatalf("lock target for first claim: %v", err)
	}
	if !locked {
		txA.Rollback(ctx)
		t.Fatal("first claim could not take the target lock")
	}
	if err := lease(txA, olderID, "first-lease"); err != nil {
		txA.Rollback(ctx)
		t.Fatalf("lease older row: %v", err)
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit first claim: %v", err)
	}

	// The second claimant now takes the released lock. Its stale candidate is
	// no longer eligible, and the session's other row is blocked by the
	// committed in-flight lease; neither may be leased.
	locked, err = queries.WithTx(txB).TryLockDeliveryTarget(ctx, targetSessionID)
	if err != nil {
		t.Fatalf("lock target for second claim: %v", err)
	}
	if !locked {
		t.Fatal("second claim could not take the target lock after the first committed")
	}
	if err := lease(txB, olderID, "second-lease"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale candidate lease error = %v, want pgx.ErrNoRows", err)
	}
	otherID := comment.ID
	if inflight.ID == comment.ID {
		otherID = generic.ID
	}
	if err := lease(txB, otherID, "other-lease"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("other row lease error = %v, want pgx.ErrNoRows", err)
	}
	if err := txB.Rollback(ctx); err != nil {
		t.Fatalf("rollback second claim: %v", err)
	}

	var inflightRows int
	var leasedID string
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(min(id::text), '')
		FROM agent_mailbox_message
		WHERE recipient_agent_id = $1
		  AND delivery_status = 'leased'
		  AND lease_expires_at > now()`, recipientUUID).Scan(&inflightRows, &leasedID); err != nil {
		t.Fatalf("count in-flight rows: %v", err)
	}
	if inflightRows != 1 || leasedID != inflight.ID {
		t.Fatalf("in-flight rows = %d (lease %s), want only %s", inflightRows, leasedID, inflight.ID)
	}
}
func TestListIssueMailboxDeliveriesIncludesGenericRows(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, _, _ := newClaimFixture(t, pool)
	issueID := uuid.NewString()
	comment := enqueueCommentForIssue(t, pool, workspaceID, agentID, issueID)

	store := NewPostgres(db.New(pool))
	generic, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "issue_wakeup", Key: "wakeup-issue-" + uuid.NewString()},
		Target:          Target{SessionID: fixtureSessionID(t, pool, agentID)},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "wakeup:run-issue:" + uuid.NewString(),
		Kind:            "issue_wakeup",
		Refs:            Refs{IssueID: issueID},
		Payload:         json.RawMessage(`{"schema":"issue_wakeup.v1","issue_id":"` + issueID + `"}`),
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
	got := map[string]bool{}
	for _, row := range rows {
		got[util.UUIDToString(row.ID)] = true
	}
	if len(rows) != 2 || !got[comment.ID] || !got[generic.ID] {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, util.UUIDToString(row.ID))
		}
		t.Fatalf("issue deliveries = %v, want the comment %s and generic %s", ids, comment.ID, generic.ID)
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
