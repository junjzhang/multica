package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func managedResultMailbox(t *testing.T, name string) (db.AgentTaskQueue, db.AgentTaskQueue, string, string) {
	t.Helper()
	source, result, _ := resultFixture(t, name)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now() WHERE id=$1`, source.ID)
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	rows, err := testHandler.Queries.ListTaskResultNotifications(context.Background(), db.ListTaskResultNotificationsParams{TaskID: result.ID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil || len(rows) != 1 || !rows[0].NotificationDeliveryID.Valid || !rows[0].NotificationTaskID.Valid {
		t.Fatalf("materialization: %+v %v", rows, err)
	}
	return source, result, uuidToString(rows[0].NotificationDeliveryID), uuidToString(rows[0].NotificationTaskID)
}

func ackManagedResult(t *testing.T, source db.AgentTaskQueue, message, state string, want int) {
	t.Helper()
	token := taskTokenFixture(t, uuidToString(source.AgentID), uuidToString(source.ID))
	response := serveAuthenticated(t, token, http.HandlerFunc(testHandler.AckAgentMailbox), http.MethodPost, "/api/agents/"+uuidToString(source.AgentID)+"/mailbox/"+message+"/ack", map[string]string{"state": state}, map[string]string{"id": uuidToString(source.AgentID), "messageId": message}, nil)
	if response.Code != want {
		t.Fatalf("ACK: %d %s", response.Code, response.Body.String())
	}
}

func TestManagedMailboxHandledCancelsQueuedWakeup(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	source, _, message, turn := managedResultMailbox(t, "mailbox-active-consumer")
	token := taskTokenFixture(t, uuidToString(source.AgentID), uuidToString(source.ID))
	listed := serveAuthenticated(t, token, http.HandlerFunc(testHandler.ListAgentMailbox), http.MethodGet, "/mailbox?kind=delegation_result", nil, map[string]string{"id": uuidToString(source.AgentID)}, nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listed.Code, listed.Body.String())
	}
	var page struct {
		Messages []MailboxMessageResponse `json:"messages"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != message || page.Messages[0].ProcessingStatus != "unread" {
		t.Fatalf("list changed consumption: %+v", page)
	}
	for _, state := range []string{"read", "handled", "handled", "read"} {
		ackManagedResult(t, source, message, state, http.StatusOK)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, source.ID)
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(context.Background(), source.RuntimeID)
	if err != nil || claimed != nil {
		t.Fatalf("handled wakeup was claimed: %+v %v", claimed, err)
	}
	stored, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(turn))
	if err != nil || stored.Status != "cancelled" {
		t.Fatalf("wakeup: %+v %v", stored, err)
	}
	receipt, err := testHandler.Queries.GetAgentMailboxMessage(context.Background(), db.GetAgentMailboxMessageParams{ID: parseUUID(message), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil || receipt.ProcessingStatus != "handled" || receipt.DeliveryStatus != "pending" {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
}

func TestManagedMailboxUnreadStillClaimsAndOtherScopeCannotConsume(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	source, _, message, turn := managedResultMailbox(t, "mailbox-scope-guard")
	otherIssue := dbfx.Issue(t, "unrelated mailbox scope")
	otherID := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": otherIssue, "runtime_id": uuidToString(source.RuntimeID), "status": "running", "session_id": source.SessionID.String, "work_dir": source.WorkDir.String, "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	other, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(otherID))
	if err != nil {
		t.Fatal(err)
	}
	ackManagedResult(t, other, message, "handled", http.StatusNotFound)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id IN ($1,$2)`, source.ID, other.ID)
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(context.Background(), source.RuntimeID)
	if err != nil || claimed == nil || uuidToString(claimed.ID) != turn {
		t.Fatalf("unread result did not claim: %+v %v", claimed, err)
	}
}

func TestManagedMailboxAckRacesStartWithoutStoppingRunningTask(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		source, _, message, turn := managedResultMailbox(t, fmt.Sprintf("mailbox-start-race-%d", i))
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, source.ID)
		claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, source.RuntimeID)
		if err != nil || claimed == nil || uuidToString(claimed.ID) != turn {
			t.Fatalf("claim: %+v %v", claimed, err)
		}
		gate := make(chan struct{})
		acked := make(chan error, 1)
		started := make(chan error, 1)
		go func() {
			<-gate
			_, err := testHandler.TaskService.AckMailbox(ctx, db.AckAgentMailboxProcessingParams{ID: parseUUID(message), WorkspaceID: parseUUID(testWorkspaceID), RecipientAgentID: source.AgentID, State: "handled"}, pgtype.UUID{})
			acked <- err
		}()
		go func() {
			<-gate
			_, err := testHandler.TaskService.StartTaskForClaim(ctx, db.LockAgentTaskStartClaimParams{ID: claimed.ID, RuntimeID: claimed.RuntimeID, DispatchedAt: claimed.DispatchedAt})
			started <- err
		}()
		close(gate)
		ackErr, startErr := <-acked, <-started
		if ackErr != nil {
			t.Fatal(ackErr)
		}
		actual, err := testHandler.Queries.GetAgentTask(ctx, claimed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if startErr == nil && actual.Status != "running" {
			t.Fatalf("ACK stopped started task: %s", actual.Status)
		}
		if startErr != nil && actual.Status != "cancelled" {
			t.Fatalf("lost start was not cancelled: %s %v", actual.Status, startErr)
		}
		// End the simulated run so it cannot obstruct later fixture cleanup.
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1 AND status='running'`, claimed.ID)
	}
}

func TestExternalMailboxHandledPreservesUnknownReceiptAndWithdrawsPending(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, state := range []string{"pending", "leased", "unknown"} {
		t.Run(state, func(t *testing.T) {
			f := externalTakeoverFixture(t)
			message := queryString(t, `SELECT notification_delivery_id::text FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2`, f.result.ID, f.address.ID)
			dbfx.Exec(t, `UPDATE agent_mailbox_message SET delivery_status=$2,host_receipt='original-receipt',lease_owner='original-daemon',lease_token_hash='original-lease' WHERE id=$1`, message, state)
			token := externalTokenFixture(t, uuidToString(f.address.DispatcherAgentID), f.owner, time.Now().Add(time.Hour))
			response := serveAuthenticated(t, token, http.HandlerFunc(testHandler.AckAgentMailbox), http.MethodPost, "/ack", map[string]string{"state": "handled"}, map[string]string{"id": uuidToString(f.address.DispatcherAgentID), "messageId": message}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("ACK: %d %s", response.Code, response.Body.String())
			}
			row, err := testHandler.Queries.GetAgentMailboxMessage(context.Background(), db.GetAgentMailboxMessageParams{ID: parseUUID(message), WorkspaceID: parseUUID(testWorkspaceID)})
			expected := state
			if state == "pending" {
				expected = "failed"
			}
			if err != nil || row.DeliveryStatus != expected || row.ProcessingStatus != "handled" || !row.WithdrawnAt.Valid || row.HostReceipt.String != "original-receipt" || row.LeaseTokenHash.String != "original-lease" {
				t.Fatalf("receipt changed: %+v %v", row, err)
			}
		})
	}
}

func TestManagedMailboxConsumerInheritsLateUpstream(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, review, message, turnID := managedResultMailbox(t, "mailbox-continuation")
	upstream := externalUpstream(t, source, "mailbox-upstream", true)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, source.ID)
	consumerID := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": uuidToString(source.IssueID), "runtime_id": uuidToString(source.RuntimeID), "status": "running", "session_id": source.SessionID.String, "work_dir": source.WorkDir.String, "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	consumer, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(consumerID))
	if err != nil {
		t.Fatal(err)
	}
	ackManagedResult(t, consumer, message, "handled", http.StatusOK)
	requireUpstreamReturns(t, consumer, upstream)
	late := externalUpstream(t, source, "mailbox-late-upstream", true)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	requireUpstreamReturns(t, consumer, upstream, late)
	if _, err := testHandler.TaskService.CompleteTask(service.WithTerminalReport(ctx), consumer.ID, []byte(`{"output":"Reviewed artifact accepted"}`), consumer.SessionID.String, consumer.WorkDir.String, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if count := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_delivery_id IS NOT NULL`, consumer.ID); count != 2 {
		t.Fatalf("consumer lost final returns: %d", count)
	}
	if count := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_delivery_id IS NOT NULL`, turnID); count != 0 {
		t.Fatalf("cancelled wakeup returned receipt as work: %d", count)
	}
	// A later review result has its own mailbox and remains runnable.
	nextID := dbfx.Task(t, uuidToString(review.AgentID), testutil.Cols{"issue_id": uuidToString(review.IssueID), "runtime_id": uuidToString(review.RuntimeID), "status": "running"})
	links, err := testHandler.Queries.GetDelegationResultObligationForTask(ctx, parseUUID(turnID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.Queries.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{TaskID: parseUUID(nextID), DelegationReturnID: links.DelegationReturnID, CallerTaskIds: []pgtype.UUID{consumer.ID}}); err != nil {
		t.Fatal(err)
	}
	next, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(nextID))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, next)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, source.RuntimeID)
	if err != nil || claimed == nil || uuidToString(claimed.ID) == turnID {
		t.Fatalf("old ACK suppressed new result: %+v %v", claimed, err)
	}
}

func TestManagedMailboxCancelledPreparedClaimReportsExactExit(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, _, message, _ := managedResultMailbox(t, "mailbox-prepared-exit")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, source.ID)
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, source.RuntimeID)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET resource_control_required=true WHERE id=$1`, claimed.ID)
	if _, err := testHandler.TaskService.AckMailbox(ctx, db.AckAgentMailboxProcessingParams{ID: parseUUID(message), WorkspaceID: parseUUID(testWorkspaceID), RecipientAgentID: source.AgentID, State: "handled"}, pgtype.UUID{}); err != nil {
		t.Fatal(err)
	}
	var rejection struct {
		CancelledClaim bool `json:"cancelled_claim"`
	}
	testutil.Call(t, testHandler.StartTask, startClaimRequest(uuidToString(claimed.ID), uuidToString(claimed.RuntimeID), claimed.DispatchedAt.Time)).Want(http.StatusConflict).JSON(&rejection)
	if !rejection.CancelledClaim {
		t.Fatal("owned cancellation was indistinguishable from stale claim")
	}
	stale := map[string]string{"runtime_id": uuidToString(claimed.RuntimeID), "dispatched_at": claimed.DispatchedAt.Time.Add(-time.Second).Format(time.RFC3339Nano)}
	testutil.Call(t, testHandler.AckTaskCancelled, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/cancel-ack", stale), "taskId", uuidToString(claimed.ID))).Want(http.StatusConflict)
	actual, err := testHandler.Queries.GetAgentTask(ctx, claimed.ID)
	if err != nil || actual.TerminalReportedAt.Valid {
		t.Fatalf("stale claim proved exit: %+v %v", actual, err)
	}
	current := map[string]string{"runtime_id": uuidToString(claimed.RuntimeID), "dispatched_at": claimed.DispatchedAt.Time.Format(time.RFC3339Nano)}
	for range 2 {
		testutil.Call(t, testHandler.AckTaskCancelled, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/cancel-ack", current), "taskId", uuidToString(claimed.ID))).Want(http.StatusNoContent)
	}
	actual, err = testHandler.Queries.GetAgentTask(ctx, claimed.ID)
	if err != nil || !actual.TerminalReportedAt.Valid {
		t.Fatalf("owned cancelled claim exit missing: %+v %v", actual, err)
	}
}

func TestManagedMailboxConsumerKeepsCallerAuthorityAndUnknownAncestry(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, review, message, turn := managedResultMailbox(t, "mailbox-provenance")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, source.ID, testUserID)
	foreign := dbfx.User(t, "foreign mailbox caller", "mailbox-authority@example.com")
	consumerID := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": uuidToString(source.IssueID), "runtime_id": uuidToString(source.RuntimeID), "status": "running", "session_id": source.SessionID.String, "work_dir": source.WorkDir.String, "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()"), "originator_user_id": foreign, "accountable_user_id": foreign})
	consumer, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(consumerID))
	if err != nil {
		t.Fatal(err)
	}
	ackManagedResult(t, consumer, message, "handled", http.StatusNotFound)
	if count := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='queued'`, turn); count != 1 {
		t.Fatal("foreign authority cancelled wakeup")
	}
	// Legacy unknown callers grant no human capability, but may still be read
	// and processed by a consumer carrying the same unattributed authority.
	dbfx.Exec(t, `UPDATE agent_task_delegation_return SET caller_task_ids=NULL WHERE task_id=$1`, review.ID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=NULL WHERE id=$1`, consumer.ID)
	ackManagedResult(t, consumer, message, "handled", http.StatusOK)
	nextID := dbfx.Task(t, uuidToString(review.AgentID), testutil.Cols{"issue_id": uuidToString(review.IssueID), "runtime_id": uuidToString(review.RuntimeID), "status": "running"})
	incoming, err := testHandler.Queries.GetDelegationResultObligationForTask(ctx, parseUUID(turn))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.Queries.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{TaskID: parseUUID(nextID), DelegationReturnID: incoming.DelegationReturnID, CallerTaskIds: []pgtype.UUID{consumer.ID}}); err != nil {
		t.Fatal(err)
	}
	next, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(nextID))
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, next)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	nextTurn := queryString(t, `SELECT notification_task_id::text FROM agent_task_delegation_return WHERE task_id=$1`, next.ID)
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(nextTurn))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := service.VerifiedDelegationResult(ctx, testHandler.Queries, task)
	if err != nil || verified == nil || !verified.UpstreamReturnsUnknown {
		t.Fatalf("consumer laundered unknown ancestry: %+v %v", verified, err)
	}
}
