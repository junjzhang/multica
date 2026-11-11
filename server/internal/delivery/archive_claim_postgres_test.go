package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// archiveAgentWithWithdrawal is the transaction the archive handler runs: the
// archive write and the withdrawal of the agent's unaccepted notifications
// commit together, so neither half can be observed alone.
func archiveAgentWithWithdrawal(t *testing.T, pool *pgxpool.Pool, agentID, archivedBy string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin archive: %v", err)
	}
	defer tx.Rollback(ctx)
	queries := db.New(pool).WithTx(tx)
	if _, err := queries.ArchiveAgent(ctx, db.ArchiveAgentParams{
		ID:         mustParseUUID(t, agentID),
		ArchivedBy: mustParseUUID(t, archivedBy),
	}); err != nil {
		t.Fatalf("archive agent: %v", err)
	}
	if _, err := queries.WithdrawAgentDeliveries(ctx, db.WithdrawAgentDeliveriesParams{
		AgentIds: []pgtype.UUID{mustParseUUID(t, agentID)},
		Reason:   pgtype.Text{String: "Agent archived before the notification was accepted", Valid: true},
	}); err != nil {
		t.Fatalf("withdraw agent deliveries: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit archive: %v", err)
	}
}

// The candidate list is only a hint. An archive committed between listing a
// candidate and leasing it must be seen by the lease itself, not just by the
// list, and the archive must leave the pending notification withdrawn.
func TestFinalLeaseRechecksArchiveCommittedAfterCandidateList(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	created := enqueueGeneric(t, pool, workspaceID, agentID, "run-archive-race")
	ctx := context.Background()

	queries := db.New(pool)
	workspaceUUID := mustParseUUID(t, workspaceID)
	ownerUUID := mustParseUUID(t, ownerID)
	sessionUUID := mustParseUUID(t, fixtureSessionID(t, pool, agentID))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim: %v", err)
	}
	defer tx.Rollback(ctx)
	candidates, err := queries.WithTx(tx).ListClaimableDeliveryIDs(ctx, db.ListClaimableDeliveryIDsParams{
		WorkspaceID: workspaceUUID,
		OwnerUserID: ownerUUID,
		DaemonID:    daemonID,
		RowLimit:    5,
	})
	if err != nil {
		t.Fatalf("list claimable: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != mustParseUUID(t, created.ID) {
		tx.Rollback(ctx)
		t.Fatalf("candidates = %+v, want the notification", candidates)
	}

	// The archive commits after the candidate snapshot but before the lease.
	archiveAgentWithWithdrawal(t, pool, agentID, ownerID)

	_, err = queries.WithTx(tx).LeaseClaimableDelivery(ctx, db.LeaseClaimableDeliveryParams{
		ID:              mustParseUUID(t, created.ID),
		WorkspaceID:     workspaceUUID,
		TargetSessionID: sessionUUID,
		OwnerUserID:     ownerUUID,
		DaemonID:        daemonID,
		LeaseTokenHash:  pgtype.Text{String: "stale-candidate-lease", Valid: true},
		LeaseOwner:      pgtype.Text{String: daemonID, Valid: true},
		LeaseExpiresAt:  pgtypeTime(time.Now().Add(time.Minute)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		t.Fatalf("lease after archive error = %v, want pgx.ErrNoRows", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback claim: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, "SELECT delivery_status FROM agent_mailbox_message WHERE id=$1", created.ID).Scan(&status); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "failed" {
		t.Fatalf("archived pending notification = %q, want failed", status)
	}
}

// A notification leased just before the archive is marked withdrawn by the
// same transaction: the host's known no-effect answer cannot put it back into
// claimable delivery and send it to an agent that no longer exists.
func TestArchiveWithdrawalSuppressesBusyRetryOnLeasedNotification(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	created := enqueueGeneric(t, pool, workspaceID, agentID, "run-archive-leased")
	ctx := context.Background()

	queue := NewQueue(db.New(pool), pool)
	claims := claimForTest(t, queue, workspaceID, daemonID, ownerID)
	if len(claims) != 1 || claims[0].ID != created.ID {
		t.Fatalf("claim = %+v, want the notification", claims)
	}

	archiveAgentWithWithdrawal(t, pool, agentID, ownerID)

	outcome, err := queue.RecordOutcome(ctx, OutcomeRequest{
		Scope:      Scope{WorkspaceID: workspaceID},
		DeliveryID: created.ID,
		DaemonID:   daemonID,
		LeaseToken: claims[0].LeaseToken,
		State:      OutcomeRetry,
		RetryClass: RetryBusy,
		Error:      "agent_busy",
	})
	if err != nil {
		t.Fatalf("record busy after archive: %v", err)
	}
	if outcome.DeliveryStatus != "failed" {
		t.Fatalf("busy after archive = %q, want failed", outcome.DeliveryStatus)
	}
	if outcome.LastError == "" {
		t.Fatalf("suppressed retry lost its withdrawal reason: %+v", outcome)
	}
	var withdrawnAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT withdrawn_at FROM agent_mailbox_message WHERE id=$1", created.ID).Scan(&withdrawnAt); err != nil {
		t.Fatalf("read withdrawal marker: %v", err)
	}
	if !withdrawnAt.Valid {
		t.Fatal("the archived notification carries no withdrawal marker")
	}
	if again := claimForTest(t, queue, workspaceID, daemonID, ownerID); len(again) != 0 {
		t.Fatalf("withdrawn notification was claimable again: %+v", again)
	}
}

// claimForTest asks for the fixture's deliverable rows once.
func claimForTest(t *testing.T, queue *Queue, workspaceID, daemonID, ownerID string) []Claim {
	t.Helper()
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return claims
}

func TestFinalLeaseRechecksSessionRevocation(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspace, agent, owner, daemon := newClaimFixture(t, pool)
	created := enqueueGeneric(t, pool, workspace, agent, "revoked-session")
	q := db.New(pool)
	params := db.ListClaimableDeliveryIDsParams{WorkspaceID: mustParseUUID(t, workspace), OwnerUserID: mustParseUUID(t, owner), DaemonID: daemon, RowLimit: 5}
	candidates, err := q.ListClaimableDeliveryIDs(t.Context(), params)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE external_agent_session SET revoked_at=now() WHERE id=$1", fixtureSessionID(t, pool, agent)); err != nil {
		t.Fatal(err)
	}
	if rows, err := q.ListClaimableDeliveryIDs(t.Context(), params); err != nil || len(rows) != 0 {
		t.Fatalf("revoked candidates=%+v err=%v", rows, err)
	}
	_, err = q.LeaseClaimableDelivery(t.Context(), db.LeaseClaimableDeliveryParams{ID: mustParseUUID(t, created.ID), WorkspaceID: params.WorkspaceID, TargetSessionID: candidates[0].TargetSessionID, OwnerUserID: params.OwnerUserID, DaemonID: daemon, LeaseTokenHash: pgtype.Text{String: "revoked-race", Valid: true}, LeaseOwner: pgtype.Text{String: daemon, Valid: true}, LeaseExpiresAt: pgtypeTime(time.Now().Add(time.Minute))})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("revoked final lease=%v", err)
	}
}
