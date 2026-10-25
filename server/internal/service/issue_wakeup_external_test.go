package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// externalWakeFixture adds one registered external session to the ordinary
// wakeup fixture. Its agent has no Multica runtime, exactly like one minted by
// CreateExternalAgent, so any managed task path would be a bug.
func externalWakeFixture(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, string, string) {
	t.Helper()
	f, s, issue, _ := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE workspace_id=$1", f.WorkspaceID)
	n := principalSeq.Add(1)
	daemonID := fmt.Sprintf("daemon-external-%d", n)
	f.Runtime(t, fmt.Sprintf("rt-external-%d", n), testutil.Cols{"owner_id": f.UserID, "daemon_id": daemonID})
	agent := f.Agent(t, fmt.Sprintf("agent-external-%d", n), "", testutil.Cols{
		"owner_id": f.UserID, "execution_kind": "external",
	})
	f.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": f.WorkspaceID, "owner_user_id": f.UserID, "agent_id": agent,
		"provider": "paseo", "host_id": fmt.Sprintf("host-%d", n), "session_id": fmt.Sprintf("session-%d", n),
		"display_name": "external session", "daemon_id": daemonID,
	})
	return f, s, issue, agent, daemonID
}

func wakeupDeliveryPattern(w pgtype.UUID) string {
	return "wakeup:" + util.UUIDToString(w) + ":%"
}

func countWakeupDeliveries(t *testing.T, f principalFixture, w pgtype.UUID, agent, status string) int {
	t.Helper()
	return f.Count(t, `SELECT count(*) FROM agent_mailbox_message
		WHERE workspace_id=$1 AND recipient_agent_id=$2 AND source_type=$3
		  AND source_key LIKE $4 AND ($5='' OR delivery_status=$5)`,
		f.WorkspaceID, agent, wakeupDeliverySourceType, wakeupDeliveryPattern(w), status)
}

func newestWakeupDelivery(t *testing.T, f principalFixture, w pgtype.UUID) db.AgentMailboxMessage {
	t.Helper()
	var row db.AgentMailboxMessage
	err := f.Pool.QueryRow(context.Background(), `SELECT id,workspace_id,issue_id,comment_id,recipient_agent_id,
			source_session_id,sender_type,sender_id,delivery_status,processing_status,lease_token_hash,lease_owner,
			lease_expires_at,attempts,next_attempt_at,delivered_at,host_receipt,last_error,created_at,updated_at,
			source_type,source_key,kind,recipient_type,payload
		FROM agent_mailbox_message
		WHERE workspace_id=$1 AND source_type=$2 AND source_key LIKE $3
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		f.WorkspaceID, wakeupDeliverySourceType, wakeupDeliveryPattern(w)).Scan(
		&row.ID, &row.WorkspaceID, &row.IssueID, &row.CommentID, &row.RecipientAgentID,
		&row.SourceSessionID, &row.SenderType, &row.SenderID, &row.DeliveryStatus, &row.ProcessingStatus,
		&row.LeaseTokenHash, &row.LeaseOwner, &row.LeaseExpiresAt, &row.Attempts, &row.NextAttemptAt,
		&row.DeliveredAt, &row.HostReceipt, &row.LastError, &row.CreatedAt, &row.UpdatedAt,
		&row.SourceType, &row.SourceKey, &row.Kind, &row.RecipientType, &row.Payload)
	if err != nil {
		t.Fatalf("read wakeup delivery: %v", err)
	}
	return row
}

// A one-shot timer aimed at an external session must produce exactly one
// durable delivery -- no managed run, no second id on a repeated scan -- and
// the rule must end as a consumed one-shot.
func TestWakeupExternalTimerEnqueuesOneDeliveryOnce(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Read the finished job", AfterSeconds: 60})

	got := wakeTick(t, f, s, w.ID)
	if got.Enabled || got.FireCount != 1 {
		t.Fatalf("once rule after firing: enabled=%t fire_count=%d", got.Enabled, got.FireCount)
	}
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("external target started %d managed runs", n)
	}
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("deliveries = %d, want 1", n)
	}
	row := newestWakeupDelivery(t, f, w.ID)
	if row.Kind != wakeupDeliveryKind || row.DeliveryStatus != "pending" {
		t.Fatalf("delivery = kind %q status %q", row.Kind, row.DeliveryStatus)
	}
	if row.CommentID.Valid {
		t.Fatalf("generic delivery fabricated a comment reference: %+v", row.CommentID)
	}
	if util.UUIDToString(row.IssueID) != util.UUIDToString(issue) {
		t.Fatalf("delivery issue = %s, want %s", util.UUIDToString(row.IssueID), util.UUIDToString(issue))
	}
	if !strings.Contains(string(row.Payload), "Read the finished job") || !strings.Contains(string(row.Payload), `"schema": "issue_wakeup.v1"`) {
		t.Fatalf("payload is not the rendered wakeup note: %s", row.Payload)
	}
	if n := f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered' AND details->>'delivery_id'=$2`, issue, util.UUIDToString(row.ID)); n != 1 {
		t.Fatalf("timeline entries linked to the delivery = %d, want 1", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); n != 0 {
		t.Fatalf("%d inputs left unprocessed after delivery", n)
	}

	// The scheduler can visit a decided rule again; the consumed inputs must
	// not mint a second delivery id.
	wakeDispatch(t, s, got)
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("deliveries after a repeated scan = %d, want 1", n)
	}
}

// The timer lives in the database, not in a process: a service instance that
// starts after the deadline still enqueues exactly one delivery.
func TestWakeupExternalTimerRecoversAfterRestart(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Check the batch", AfterSeconds: 60})
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", w.ID)

	restarted := &IssueWakeupService{Tasks: f.svc.TaskSvc}
	drainTick(t, restarted, parseTestUUID(t, f.WorkspaceID))
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("after restart deliveries = %d, want 1", n)
	}
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("after restart the external target started %d runs", n)
	}
}

// Several inputs captured for one firing are one notification, and a replay of
// that firing resolves the delivery that already exists.
func TestWakeupExternalEventMergesInputsIntoOneDelivery(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "Read the new comments"})
	f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload) VALUES
		(gen_random_uuid(),$1,$2,'comment-1','comment.created','{"event_id":"comment-1"}'),
		(gen_random_uuid(),$1,$2,'comment-2','comment.created','{"event_id":"comment-2"}')`, w.ID, w.Revision)

	wakeDispatch(t, s, w)
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("deliveries = %d, want one merged notification", n)
	}
	row := newestWakeupDelivery(t, f, w.ID)
	for _, want := range []string{"comment-1", "comment-2"} {
		if !strings.Contains(string(row.Payload), want) {
			t.Fatalf("merged payload missing %s: %s", want, row.Payload)
		}
	}
	wakeDispatch(t, s, w)
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("deliveries after replay = %d, want 1", n)
	}
}

// A busy session must not lose or duplicate the notification: the producer
// queues it, the claim path keeps it pending behind the in-flight delivery,
// and it becomes claimable once that delivery settles.
func TestWakeupExternalDeliveryWaitsBehindBusySession(t *testing.T) {
	f, s, issue, agent, daemonID := externalWakeFixture(t)
	ctx := context.Background()
	// Pretend the host is still working on an earlier notification.
	f.Exec(t, `INSERT INTO agent_mailbox_message(id,workspace_id,recipient_agent_id,recipient_type,source_type,source_key,kind,payload,
			delivery_status,lease_token_hash,lease_owner,lease_expires_at,attempts)
		VALUES (gen_random_uuid(),$1,$2,'agent','issue_wakeup','wakeup:busy','issue_wakeup','{}'::jsonb,
			'leased','hash',$3,now()+interval '5 minutes',1)`, f.WorkspaceID, agent, daemonID)

	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Check it now", AfterSeconds: 60})
	wakeTick(t, f, s, w.ID)

	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 1 {
		t.Fatalf("new delivery = %d pending, want it queued", n)
	}
	claim := func() []delivery.Claim {
		t.Helper()
		claims, err := delivery.NewQueue(f.q, f.Pool).Claim(ctx, delivery.ClaimRequest{
			Scope: delivery.Scope{WorkspaceID: f.WorkspaceID}, DaemonID: daemonID,
			OwnerUserID: f.UserID, Limit: 5, Projection: delivery.GenericProjection,
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return claims
	}
	if claims := claim(); len(claims) != 0 {
		t.Fatalf("busy session leased %d deliveries, want 0", len(claims))
	}
	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 1 {
		t.Fatalf("delivery left pending = %d", n)
	}

	// The earlier delivery settles; the queued wakeup notification is now the
	// one claim the bound daemon may lease.
	f.Exec(t, `UPDATE agent_mailbox_message SET delivery_status='delivered',delivered_at=now()
		WHERE workspace_id=$1 AND source_key='wakeup:busy'`, f.WorkspaceID)
	claims := claim()
	if len(claims) != 1 || claims[0].Kind != wakeupDeliveryKind {
		t.Fatalf("claims = %+v, want the one wakeup delivery", claims)
	}
	if claims[0].Session.DaemonID != daemonID || claims[0].Session.SessionID == "" {
		t.Fatalf("claim session = %+v", claims[0].Session)
	}
	if !strings.Contains(string(claims[0].Payload), "Check it now") {
		t.Fatalf("claim payload = %s", claims[0].Payload)
	}
}

// Disabling a rule withdraws a notification the host never accepted, and two
// scan paths stay consistent: no delivery before the trigger, the queued one
// failed with a visible reason after it.
func TestWakeupExternalDisableWithdrawsUnacceptedDelivery(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	ctx := context.Background()
	member := parseTestUUID(t, f.UserID)

	// Cancelled before its time: nothing is enqueued.
	early := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Never mind", AfterSeconds: 120})
	if _, err := s.Disable(ctx, issue, early.ID, member); err != nil {
		t.Fatal(err)
	}
	wakeDispatch(t, s, early)
	if n := countWakeupDeliveries(t, f, early.ID, agent, ""); n != 0 {
		t.Fatalf("a disabled rule enqueued %d deliveries", n)
	}

	// Cancelled after it fired: the queued notification is withdrawn rather
	// than waking the session for a rule the member ended.
	fired := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Run the checks", AfterSeconds: 60})
	wakeTick(t, f, s, fired.ID)
	if n := countWakeupDeliveries(t, f, fired.ID, agent, "pending"); n != 1 {
		t.Fatalf("pending deliveries before disable = %d", n)
	}
	if _, err := s.Disable(ctx, issue, fired.ID, member); err != nil {
		t.Fatal(err)
	}
	if n := countWakeupDeliveries(t, f, fired.ID, agent, "pending"); n != 0 {
		t.Fatalf("disable left %d deliveries pending", n)
	}
	row := newestWakeupDelivery(t, f, fired.ID)
	if row.DeliveryStatus != "failed" || !row.LastError.Valid || !strings.Contains(row.LastError.String, "Wakeup disabled") {
		t.Fatalf("withdrawn delivery = status %q error %+v", row.DeliveryStatus, row.LastError)
	}
}

// A lease the host may already have acted on is never withdrawn: cancellation
// only stops what was provably not accepted.
func TestWakeupExternalDisableKeepsLeasedDelivery(t *testing.T) {
	f, s, issue, agent, daemonID := externalWakeFixture(t)
	ctx := context.Background()
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Check it", AfterSeconds: 60})
	wakeTick(t, f, s, w.ID)
	f.Exec(t, `UPDATE agent_mailbox_message SET delivery_status='leased',lease_token_hash='hash',lease_owner=$2,
		lease_expires_at=now()+interval '5 minutes' WHERE workspace_id=$1 AND source_type='issue_wakeup'`, f.WorkspaceID, daemonID)

	if _, err := s.Disable(ctx, issue, w.ID, parseTestUUID(t, f.UserID)); err != nil {
		t.Fatal(err)
	}
	row := newestWakeupDelivery(t, f, w.ID)
	if row.DeliveryStatus != "leased" || row.LastError.Valid {
		t.Fatalf("leased delivery after disable = status %q error %+v", row.DeliveryStatus, row.LastError)
	}
}

// An external target without a registered host session cannot be delivered,
// so registration refuses it instead of queueing notifications forever.
func TestWakeupExternalTargetRequiresRegisteredSession(t *testing.T) {
	f, s, issue, _, _ := externalWakeFixture(t)
	n := principalSeq.Add(1)
	unbound := f.Agent(t, fmt.Sprintf("agent-unbound-%d", n), "", testutil.Cols{"owner_id": f.UserID, "execution_kind": "external"})
	if _, err := s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), pgtype.UUID{},
		WakeupInput{AgentID: unbound, Kind: "at", Instruction: "Check it", AfterSeconds: 60}); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("registration error = %v, want ErrWakeupInput", err)
	}
}

// The managed route stays a managed run: the only change is where an external
// target's firing lands.
func TestWakeupManagedTargetStillStartsRun(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", Instruction: "Check it", AfterSeconds: 60})
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("managed target runs = %d, want 1", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM agent_mailbox_message WHERE workspace_id=$1 AND source_type=$2`, f.WorkspaceID, wakeupDeliverySourceType); n != 0 {
		t.Fatalf("managed target enqueued %d generic deliveries", n)
	}
}

// A checks_finished condition drives the same delivery path, with the terminal
// facts (snapshot head and conclusion) in the notification.
func TestWakeupExternalPRChecksConditionEnqueuesDelivery(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	s.Tasks.PRSnapshotsEnabled = true
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "checks", "pr_number": 9,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/9",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": "a", "snapshot_head_sha": "a", "checks_rollup_state": "PENDING",
	})
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Read the CI result",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})

	wakeTick(t, f, s, w.ID)
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 0 {
		t.Fatalf("pending checks enqueued %d deliveries", n)
	}
	snapshot := func(head, checks string) {
		f.Exec(t, "UPDATE github_pull_request SET head_sha=$2,snapshot_head_sha=$2,checks_rollup_state=$3 WHERE id=$1", pr, head, checks)
		wakeTick(t, f, s, w.ID)
	}
	snapshot("a", "SUCCESS")
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("finished checks enqueued %d deliveries, want 1", n)
	}
	payload := string(newestWakeupDelivery(t, f, w.ID).Payload)
	if !strings.Contains(payload, `\"head_sha\":\"a\"`) || !strings.Contains(payload, `\"checks\":\"success\"`) {
		t.Fatalf("delivery does not carry the terminal CI facts: %s", payload)
	}
	// The same finished head is not a second terminal event.
	snapshot("a", "SUCCESS")
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("same head enqueued %d deliveries, want 1", n)
	}
	// A new head with a new outcome is one more notification, once the host has
	// taken the earlier one: an undecided notification holds the new input.
	snapshot("b", "FAILURE")
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("new head overtook an undecided delivery: %d deliveries, want 1", n)
	}
	settleWakeupDeliveries(t, f, w.ID)
	snapshot("b", "FAILURE")
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 2 {
		t.Fatalf("new head enqueued %d deliveries in total, want 2", n)
	}
	if !strings.Contains(string(newestWakeupDelivery(t, f, w.ID).Payload), `\"checks\":\"failure\"`) {
		t.Fatalf("new delivery does not carry the failure")
	}
}

// settleWakeupDeliveries records the outcome a host would report, so the next
// firing may produce a new notification.
func settleWakeupDeliveries(t *testing.T, f principalFixture, w pgtype.UUID) {
	t.Helper()
	f.Exec(t, `UPDATE agent_mailbox_message SET delivery_status='delivered',delivered_at=now()
		WHERE workspace_id=$1 AND source_type=$2 AND source_key LIKE $3 AND delivery_status<>'delivered'`,
		f.WorkspaceID, wakeupDeliverySourceType, wakeupDeliveryPattern(w))
}

// A busy or offline host must not turn every elapsed period into its own model
// turn. While one notification for the rule is undecided, later firings keep
// their inputs; after it settles, the scheduler merges them into a single new
// notification. No managed run is started at any point.
func TestWakeupExternalEveryWaitsForUndeliveredFiring(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "every", IntervalSeconds: 60, Instruction: "Check the job"})

	wakeTick(t, f, s, w.ID)
	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 1 {
		t.Fatalf("first firing deliveries = %d pending, want 1", n)
	}

	// The host stays busy across three more periods.
	for period := 0; period < 3; period++ {
		got := wakeTick(t, f, s, w.ID)
		if got.FireCount != 1 {
			t.Fatalf("firing %d: fire_count = %d, a pending notification must hold the inputs", period+2, got.FireCount)
		}
	}
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("deliveries after three waiting periods = %d, want 1", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); n != 1 {
		t.Fatalf("unprocessed inputs = %d, want the latest timer input retained", n)
	}
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("waiting for a delivery started %d managed runs", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM agent_mailbox_message WHERE workspace_id=$1 AND recipient_agent_id=$2
		AND source_type=$3 AND created_at > now()-interval '1 hour'`, f.WorkspaceID, agent, wakeupDeliverySourceType); n != 1 {
		t.Fatalf("deliveries in the last hour = %d, want at most 1 while waiting", n)
	}

	// Once the host takes the notification, the next firing delivers the
	// accumulated input as exactly one more notification.
	settleWakeupDeliveries(t, f, w.ID)
	got := wakeTick(t, f, s, w.ID)
	if got.FireCount != 2 {
		t.Fatalf("fire_count after the held input was delivered = %d, want 2", got.FireCount)
	}
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 2 {
		t.Fatalf("deliveries after settling = %d, want 2", n)
	}
	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 1 {
		t.Fatalf("pending deliveries after settling = %d, want 1", n)
	}
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("external target started %d managed runs", n)
	}
}

// The same rule-level merge applies to an event burst: a second event that
// arrives while the first notification is undecided stays an input, not a
// second notification. The next pass after the delivery settles produces one
// notification carrying the retained input.
func TestWakeupExternalEventBurstWaitsForUndeliveredFiring(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous",
		EventTypes: []string{"comment.created"}, Instruction: "Read the new comments"})
	addReceipt := func(key string) {
		t.Helper()
		f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload)
			VALUES (gen_random_uuid(),$1,$2,$3,'comment.created',jsonb_build_object('event_id',$3::text))`, w.ID, w.Revision, key)
	}

	addReceipt("comment-1")
	wakeDispatch(t, s, w)
	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 1 {
		t.Fatalf("first event deliveries = %d pending, want 1", n)
	}

	addReceipt("comment-2")
	wakeDispatch(t, s, w)
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 1 {
		t.Fatalf("a second event while the first is pending made %d deliveries, want 1", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); n != 1 {
		t.Fatalf("retained inputs = %d, want the second event kept for the next firing", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id=$1 AND fire_count=1`, w.ID); n != 1 {
		t.Fatal("fire_count while waiting is not 1")
	}

	settleWakeupDeliveries(t, f, w.ID)
	wakeDispatch(t, s, w)
	if n := countWakeupDeliveries(t, f, w.ID, agent, ""); n != 2 {
		t.Fatalf("deliveries after the first settled = %d, want 2", n)
	}
	if payload := string(newestWakeupDelivery(t, f, w.ID).Payload); !strings.Contains(payload, "comment-2") {
		t.Fatalf("second delivery does not carry the retained input: %s", payload)
	}
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("external target started %d managed runs", n)
	}
}

// A host session can disappear after the rule was registered. Dispatching then
// must not queue a notification nobody can claim: the rule ends with a visible
// reason and its unaccepted notification is withdrawn.
func TestWakeupExternalDispatchDisablesWhenSessionDisappears(t *testing.T) {
	f, s, issue, agent, _ := externalWakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "every", IntervalSeconds: 60, Instruction: "Check the job"})

	wakeTick(t, f, s, w.ID)
	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 1 {
		t.Fatalf("pending deliveries before the session disappeared = %d, want 1", n)
	}

	f.Exec(t, `DELETE FROM external_agent_session WHERE agent_id=$1`, agent)
	got := wakeTick(t, f, s, w.ID)
	if got.Enabled || !got.DisabledAt.Valid {
		t.Fatalf("rule after its session disappeared: enabled=%t disabled_at=%+v", got.Enabled, got.DisabledAt)
	}
	if !got.LastError.Valid || !strings.Contains(got.LastError.String, "external host session") {
		t.Fatalf("rule error = %+v, want the missing host session", got.LastError)
	}
	if n := countWakeupDeliveries(t, f, w.ID, agent, "pending"); n != 0 {
		t.Fatalf("a rule without a delivery address left %d pending notifications", n)
	}
	row := newestWakeupDelivery(t, f, w.ID)
	if row.DeliveryStatus != "failed" || !row.LastError.Valid || !strings.Contains(row.LastError.String, "host session") {
		t.Fatalf("withdrawn delivery = status %q error %+v", row.DeliveryStatus, row.LastError)
	}
}
