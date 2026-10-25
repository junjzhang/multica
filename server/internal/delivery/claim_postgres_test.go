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
	"github.com/jackc/pgx/v5/pgxpool"
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

func enqueueGeneric(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, key string) Delivery {
	t.Helper()
	store := NewPostgres(db.New(pool))
	created, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:     Scope{WorkspaceID: workspaceID},
		Source:    Source{Type: "ci_run", Key: key},
		Recipient: Recipient{Type: "agent", ID: agentID},
		Kind:      "ci_terminal",
		Payload:   json.RawMessage(`{"run_id":"run-42","conclusion":"success"}`),
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
	commentID := uuid.NewString()
	issueID := uuid.NewString()
	store := NewPostgres(db.New(pool))
	created, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:     Scope{WorkspaceID: workspaceID},
		Source:    Source{Type: "comment", Key: commentID},
		Recipient: Recipient{Type: "agent", ID: agentID},
		Kind:      "comment_mention",
		Payload:   json.RawMessage(fmt.Sprintf(`{"schema":"comment_mention.v1","issue_id":%q,"comment_id":%q}`, issueID, commentID)),
		Refs:      Refs{IssueID: issueID, CommentID: commentID},
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
