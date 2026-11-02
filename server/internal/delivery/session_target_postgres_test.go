package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// addFixtureSession registers a second (or later) physical session for the
// same agent. daemonID selects the worker that may reach it.
func addFixtureSession(t *testing.T, pool *pgxpool.Pool, workspaceID, ownerID, agentID, daemonID string) string {
	t.Helper()
	suffix := uuid.NewString()
	sessionID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO external_agent_session
			(id, workspace_id, owner_user_id, agent_id, provider, host_id, session_id, display_name, daemon_id)
		VALUES ($1, $2, $3, $4, 'paseo', $5, $6, $7, $8)`,
		sessionID, workspaceID, ownerID, agentID, "host-"+suffix, "session-"+suffix, "session "+suffix, daemonID); err != nil {
		t.Fatalf("insert external session: %v", err)
	}
	return sessionID
}

func addFixtureIssue(t *testing.T, pool *pgxpool.Pool, workspaceID, ownerID string) string {
	t.Helper()
	issueID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO issue (id, workspace_id, number, title, creator_type, creator_id)
		VALUES ($1, $2, (SELECT COALESCE(max(number), 0) + 1 FROM issue WHERE workspace_id = $2), $3, 'member', $4)`,
		issueID, workspaceID, "delivery issue "+issueID, ownerID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	return issueID
}

// Two sessions of one agent are independent targets: one claim round leases
// both, even though the business recipient is the same agent.
func TestClaimLeasesTwoSessionsOfOneAgentIndependently(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	first := fixtureSessionID(t, pool, agentID)
	second := addFixtureSession(t, pool, workspaceID, ownerID, agentID, daemonID)

	createdFirst := enqueueGenericTo(t, pool, workspaceID, agentID, first, "run-session-a")
	createdSecond := enqueueGenericTo(t, pool, workspaceID, agentID, second, "run-session-b")

	queue := NewQueue(db.New(pool), pool)
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
		Limit:       5,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %d, want one per session", len(claims))
	}
	targets := map[string]string{}
	for _, claim := range claims {
		targets[claim.ID] = claim.Delivery.Target.SessionID
	}
	if targets[createdFirst.ID] != first || targets[createdSecond.ID] != second {
		t.Fatalf("claim targets = %+v, want %s->%s and %s->%s", targets, createdFirst.ID, first, createdSecond.ID, second)
	}
}

// The same physical session serializes across issues: a message for issue A in
// flight holds the session's slot, so issue B's message waits.
func TestClaimSerializesOneSessionAcrossIssues(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	sessionID := fixtureSessionID(t, pool, agentID)
	issueA := addFixtureIssue(t, pool, workspaceID, ownerID)
	issueB := addFixtureIssue(t, pool, workspaceID, ownerID)

	first := enqueueCommentForIssueTo(t, pool, workspaceID, agentID, sessionID, issueA)
	second := enqueueCommentForIssueTo(t, pool, workspaceID, agentID, sessionID, issueB)

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
	if len(claims) != 1 || claims[0].ID != first.ID {
		t.Fatalf("first claim = %+v, want only the older row %s", claims, first.ID)
	}
	if again, err := queue.Claim(context.Background(), request); err != nil || len(again) != 0 {
		t.Fatalf("second claim = %+v err=%v, want the session's second message held back", again, err)
	}
	if _, err := queue.RecordOutcome(context.Background(), OutcomeRequest{
		Scope:      Scope{WorkspaceID: workspaceID},
		DeliveryID: claims[0].ID,
		DaemonID:   daemonID,
		LeaseToken: claims[0].LeaseToken,
		State:      OutcomeDelivered,
	}); err != nil {
		t.Fatalf("record delivered: %v", err)
	}
	claims, err = queue.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(claims) != 1 || claims[0].ID != second.ID {
		t.Fatalf("third claim = %+v, want %s after the session freed", claims, second.ID)
	}
}

// A legacy row without a provable destination keeps its id and receipt but is
// never leased, so no worker guesses an address for it.
func TestClaimSkipsLegacyRowWithoutFrozenTarget(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	legacyID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO agent_mailbox_message
			(id, workspace_id, recipient_agent_id, recipient_type, source_type, source_key,
			 kind, payload, delivery_status, last_error)
		VALUES ($1, $2, $3, 'agent', 'comment', 'legacy-key', 'comment_mention',
			'{"schema":"comment_mention.v1"}'::jsonb, 'pending',
			'legacy delivery has no registered external session')`,
		legacyID, workspaceID, agentID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_mailbox_message WHERE workspace_id = $1`, workspaceID)
	})

	queue := NewQueue(db.New(pool), pool)
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %+v, want the legacy row left untouched", claims)
	}
	var status, reason string
	if err := pool.QueryRow(context.Background(),
		`SELECT delivery_status, last_error FROM agent_mailbox_message WHERE id = $1`, legacyID).Scan(&status, &reason); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if status != "pending" || reason == "" {
		t.Fatalf("legacy row = %s/%q, want pending with a visible reason and its original id", status, reason)
	}
}

// Rebinding changes where future notifications go. The already-enqueued row
// keeps its original target and stays claimable only by the worker that can
// reach that session.
func TestRebindKeepsExistingDeliveryOnOriginalSession(t *testing.T) {
	pool := openDeliveryTestPool(t)
	ctx := context.Background()
	workspaceID, agentID, ownerID, daemonA := newClaimFixture(t, pool)
	sessionA := fixtureSessionID(t, pool, agentID)
	daemonB := daemonA + "-b"
	sessionB := addFixtureSession(t, pool, workspaceID, ownerID, agentID, daemonB)
	issueID := addFixtureIssue(t, pool, workspaceID, ownerID)

	queries := db.New(pool)
	binding, err := sessionroute.Bind(ctx, queries, sessionroute.BindRequest{
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		IssueID:     issueID,
		SessionID:   sessionA,
	})
	if err != nil {
		t.Fatalf("bind session A: %v", err)
	}
	old, err := NewPostgres(queries).Enqueue(ctx, EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "comment", Key: "comment-1"},
		Target:          Target{SessionID: binding.SessionID},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "comment:comment-1:agent:" + agentID,
		Kind:            "comment_mention",
		Payload:         json.RawMessage(`{"schema":"comment_mention.v1"}`),
		Refs:            Refs{IssueID: issueID},
	})
	if err != nil {
		t.Fatalf("enqueue before rebind: %v", err)
	}

	rebound, err := sessionroute.Bind(ctx, queries, sessionroute.BindRequest{
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		IssueID:     issueID,
		SessionID:   sessionB,
	})
	if err != nil {
		t.Fatalf("rebind session B: %v", err)
	}
	if rebound.SessionID != sessionB {
		t.Fatalf("rebound session = %s, want %s", rebound.SessionID, sessionB)
	}
	next, err := NewPostgres(queries).Enqueue(ctx, EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "comment", Key: "comment-2"},
		Target:          Target{SessionID: rebound.SessionID},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "comment:comment-2:agent:" + agentID,
		Kind:            "comment_mention",
		Payload:         json.RawMessage(`{"schema":"comment_mention.v1"}`),
		Refs:            Refs{IssueID: issueID},
	})
	if err != nil {
		t.Fatalf("enqueue after rebind: %v", err)
	}

	stored, err := NewPostgres(queries).Get(ctx, Scope{WorkspaceID: workspaceID}, old.ID)
	if err != nil {
		t.Fatalf("reload old delivery: %v", err)
	}
	if stored.Target.SessionID != sessionA {
		t.Fatalf("old delivery target = %s, want the frozen %s", stored.Target.SessionID, sessionA)
	}

	queue := NewQueue(queries, pool)
	claimsA, err := queue.Claim(ctx, ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonA,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim daemon A: %v", err)
	}
	if len(claimsA) != 1 || claimsA[0].ID != old.ID || claimsA[0].Delivery.Target.SessionID != sessionA {
		t.Fatalf("daemon A claims = %+v, want only the old %s on session A", claimsA, old.ID)
	}
	claimsB, err := queue.Claim(ctx, ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonB,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim daemon B: %v", err)
	}
	if len(claimsB) != 1 || claimsB[0].ID != next.ID || claimsB[0].Delivery.Target.SessionID != sessionB {
		t.Fatalf("daemon B claims = %+v, want only the new %s on session B", claimsB, next.ID)
	}
}

// Reassigning which worker can reach a session changes only adapter
// qualification: the frozen target and delivery id survive.
func TestDaemonReassignmentKeepsTargetAndDeliveryID(t *testing.T) {
	pool := openDeliveryTestPool(t)
	ctx := context.Background()
	workspaceID, agentID, ownerID, daemonA := newClaimFixture(t, pool)
	sessionID := fixtureSessionID(t, pool, agentID)
	created := enqueueGenericTo(t, pool, workspaceID, agentID, sessionID, "run-reassign")

	daemonB := daemonA + "-reassigned"
	if _, err := pool.Exec(ctx, `UPDATE external_agent_session SET daemon_id = $1 WHERE id = $2`, daemonB, sessionID); err != nil {
		t.Fatalf("reassign session daemon: %v", err)
	}
	queue := NewQueue(db.New(pool), pool)
	oldClaims, err := queue.Claim(ctx, ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonA,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim old daemon: %v", err)
	}
	if len(oldClaims) != 0 {
		t.Fatalf("old daemon claims = %+v, want none after reassignment", oldClaims)
	}
	newClaims, err := queue.Claim(ctx, ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonB,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim new daemon: %v", err)
	}
	if len(newClaims) != 1 || newClaims[0].ID != created.ID {
		t.Fatalf("new daemon claims = %+v, want the original delivery %s", newClaims, created.ID)
	}
	if newClaims[0].Delivery.Target.SessionID != sessionID {
		t.Fatalf("claim target = %s, want frozen %s", newClaims[0].Delivery.Target.SessionID, sessionID)
	}
	if newClaims[0].Session.DaemonID != daemonB {
		t.Fatalf("claim adapter daemon = %s, want %s", newClaims[0].Session.DaemonID, daemonB)
	}
}

// An archived recipient agent cannot receive queued work even if the transport
// row is still pending.
func TestClaimSkipsArchivedAgent(t *testing.T) {
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	enqueueGeneric(t, pool, workspaceID, agentID, "run-archived")
	if _, err := pool.Exec(context.Background(), `UPDATE agent SET archived_at = now() WHERE id = $1`, agentID); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	queue := NewQueue(db.New(pool), pool)
	claims, err := queue.Claim(context.Background(), ClaimRequest{
		Scope:       Scope{WorkspaceID: workspaceID},
		DaemonID:    daemonID,
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %+v, want none for an archived agent", claims)
	}
}

// enqueueCommentForIssueTo enqueues a comment notification for an explicit
// target session.
func enqueueCommentForIssueTo(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, targetSessionID, issueID string) Delivery {
	t.Helper()
	commentID := uuid.NewString()
	created, err := NewPostgres(db.New(pool)).Enqueue(context.Background(), EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "comment", Key: commentID},
		Target:          Target{SessionID: targetSessionID},
		Recipient:       Recipient{Type: "agent", ID: agentID},
		NotificationKey: "comment:" + commentID + ":agent:" + agentID,
		Kind:            "comment_mention",
		Payload:         json.RawMessage(`{"schema":"comment_mention.v1"}`),
		Refs:            Refs{IssueID: issueID, CommentID: commentID},
	})
	if err != nil {
		t.Fatalf("enqueue comment delivery: %v", err)
	}
	return created
}

var _ = util.MustParseUUID
