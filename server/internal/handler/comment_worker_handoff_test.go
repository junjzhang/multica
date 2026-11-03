package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func claimWorkerReplyRun(t *testing.T, runtimeID string) *AgentTaskResponse {
	t.Helper()
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
	req = withURLParam(req, "runtimeId", runtimeID)
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityCoalescedCommentsV1)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&response)
	return response.Task
}

func completeWorkerReplyRun(t *testing.T, taskID string) {
	t.Helper()
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/complete", map[string]any{"output": "Processed the inputs delivered to this run"})
	req = withURLParam(req, "taskId", taskID)
	testutil.Call(t, testHandler.CompleteTask, req).Want(http.StatusOK)
}

// A worker's explicit @squad handoff must reach the leader in every leader-run
// state; its final reply must also reach a leader run even if the first wake was
// claimed before the reply arrived.
func TestWorkerReplyDelivery(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, state := range []string{"queued", "dispatched", "running"} {
		t.Run(fmt.Sprintf("%s/explicit", state), func(t *testing.T) {
			ctx := context.Background()
			leaderRuntimeID := dbfx.Runtime(t, "Worker handoff leader runtime")
			leaderID := dbfx.Agent(t, "Worker handoff leader", leaderRuntimeID, testutil.Cols{"max_concurrent_tasks": 3})
			workerRuntimeID := dbfx.Runtime(t, "Worker handoff worker runtime")
			workerID := dbfx.Agent(t, "Worker handoff worker", workerRuntimeID)
			squadID := dbfx.Squad(t, "Worker handoff squad", leaderID)
			dbfx.SquadMember(t, squadID, "agent", workerID)
			issueID := dbfx.Issue(t, "Worker results must reach the coordinator", testutil.Cols{
				"status": "in_progress", "assignee_type": "squad", "assignee_id": squadID,
			})
			sourceID := dbfx.Task(t, leaderID, testutil.Cols{
				"runtime_id": leaderRuntimeID, "issue_id": issueID, "status": "completed",
				"is_leader_task": true, "squad_id": squadID,
				"originator_user_id": testUserID, "accountable_user_id": testUserID,
				"session_id": "worker-handoff-leader-session", "work_dir": "/work/worker-handoff-leader",
			})
			rootID := dbfx.Comment(t, issueID, fmt.Sprintf("[@Worker](mention://agent/%s) verify the implementation", workerID), testutil.Cols{
				"author_type": "agent", "author_id": leaderID, "source_task_id": sourceID,
			})
			workerTaskID := dbfx.Task(t, workerID, testutil.Cols{
				"runtime_id": workerRuntimeID, "issue_id": issueID, "status": "running",
				"trigger_comment_id": rootID, "squad_id": squadID, "delegated_from_task_id": sourceID,
				"originator_user_id": testUserID, "accountable_user_id": testUserID,
				"session_id": "worker-handoff-worker-session", "work_dir": "/work/worker-handoff-worker",
			})
			post := func(content string) CommentResponse {
				req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
					"content": content, "parent_id": rootID,
				}), "id", issueID)
				req.Header.Set("X-Agent-ID", workerID)
				req.Header.Set("X-Task-ID", workerTaskID)
				req.Header.Set("X-Actor-Source", "task_token")
				var response CommentResponse
				testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&response)
				if response.AuthorType != "agent" || response.SourceTaskID == nil || *response.SourceTaskID != workerTaskID {
					t.Fatal("worker reply lost its authenticated source task")
				}
				return response
			}
			// Explicit @ is the only handoff trigger: the progress comment opens
			// the leader run, and the result comment hands the outcome back.
			progress := post(fmt.Sprintf("[@Squad](mention://squad/%s) Verification started; final results will follow in this thread", squadID))
			var leaderTaskID string
			dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, issueID, leaderID).Scan(&leaderTaskID)
			var first *AgentTaskResponse
			if state != "queued" {
				first = claimWorkerReplyRun(t, leaderRuntimeID)
				if first == nil || first.ID != leaderTaskID || !slices.Contains(first.DeliveredCommentIDs, progress.ID) {
					t.Fatal("first leader run did not receive the worker's progress")
				}
			}
			if state == "running" {
				if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(leaderTaskID)); err != nil {
					t.Fatal(err)
				}
			}
			content := fmt.Sprintf("[@Squad](mention://squad/%s) Verification complete: found a correctness issue; please coordinate the repair", squadID)
			details := post(fmt.Sprintf("[@Squad](mention://squad/%s) Evidence: the final reply can arrive after the leader claims its inputs", squadID))
			result := post(content)
			if state == "queued" {
				first = claimWorkerReplyRun(t, leaderRuntimeID)
				if first == nil || first.ID != leaderTaskID || !slices.Contains(first.DeliveredCommentIDs, result.ID) || !slices.Contains(first.DeliveredCommentIDs, details.ID) || first.TriggerCommentContent != content {
					t.Fatal("queued leader must coalesce and receive the worker result")
				}
			} else if slices.Contains(first.DeliveredCommentIDs, result.ID) {
				t.Fatal("an earlier claim cannot have delivered a later comment")
			}
			if state != "running" {
				if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(leaderTaskID)); err != nil {
					t.Fatal(err)
				}
			}
			if other := claimWorkerReplyRun(t, leaderRuntimeID); other != nil {
				t.Fatal("same issue/leader must remain serialized")
			}
			completeWorkerReplyRun(t, leaderTaskID)
			next := claimWorkerReplyRun(t, leaderRuntimeID)
			if state == "queued" {
				if next != nil {
					t.Fatalf("already delivered result must not create another leader run: %+v", next)
				}
				return
			}
			if next == nil {
				t.Fatal("worker result persisted but was neither delivered nor followed by another leader run")
			}
			if !next.IsLeaderTask || !slices.Contains(next.DeliveredCommentIDs, result.ID) || !slices.Contains(next.DeliveredCommentIDs, details.ID) || next.TriggerCommentContent != content {
				t.Fatalf("follow-up must deliver the result in the squad leader role: leader=%v trigger=%q delivered=%v want result=%s details=%s",
					next.IsLeaderTask, next.TriggerCommentContent, next.DeliveredCommentIDs, result.ID, details.ID)
			}
			successor, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(next.ID))
			if err != nil {
				t.Fatal(err)
			}
			if successor.SquadID != parseUUID(squadID) || successor.OriginatorUserID != parseUUID(testUserID) || successor.AccountableUserID != parseUUID(testUserID) || successor.DelegatedFromTaskID != parseUUID(workerTaskID) {
				t.Fatal("worker follow-up lost squad or human delegation provenance")
			}
			if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(next.ID)); err != nil {
				t.Fatal(err)
			}
			completeWorkerReplyRun(t, next.ID)
			if another := claimWorkerReplyRun(t, leaderRuntimeID); another != nil {
				t.Fatal("worker result must not generate an endless leader follow-up loop")
			}
		})
	}
}

// Completion reconcile must replay only live, explicit handoffs. An
// unmentioned reply, a /note, a suppressed target, a leader's own @squad and an
// archived target must not create a completion-driven run merely because the
// comment falls inside the completing task's thread/time window.
func TestWorkerReplyReconcileBoundaries(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, mode := range []string{"explicit_deferred", "unplanned", "suppressed", "leader_reply", "note"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			runtimeID := dbfx.Runtime(t, "Worker replay boundary leader")
			leaderID := dbfx.Agent(t, "Worker replay boundary leader", runtimeID)
			workerRuntimeID := dbfx.Runtime(t, "Worker replay boundary worker")
			workerID := dbfx.Agent(t, "Worker replay boundary worker", workerRuntimeID)
			squadID := dbfx.Squad(t, "Worker replay boundary squad", leaderID)
			dbfx.SquadMember(t, squadID, "agent", workerID)
			issueID := dbfx.Issue(t, "Worker replay boundary", testutil.Cols{
				"status": "in_progress", "assignee_type": "squad", "assignee_id": squadID,
			})
			rootID := dbfx.Comment(t, issueID, "Coordinate this work", testutil.Cols{"created_at": testutil.Raw("now() - interval '6 minutes'")})
			taskID := dbfx.Task(t, leaderID, testutil.Cols{
				"runtime_id": runtimeID, "issue_id": issueID, "status": "queued",
				"trigger_comment_id": rootID, "is_leader_task": true, "squad_id": squadID,
				"originator_user_id": testUserID, "accountable_user_id": testUserID,
				"created_at": testutil.Raw("now() - interval '5 minutes'"),
				"session_id": "worker-replay-leader-session", "work_dir": "/work/worker-replay-leader",
			})
			workerTaskID := dbfx.Task(t, workerID, testutil.Cols{
				"runtime_id": workerRuntimeID, "issue_id": issueID, "status": "running",
				"trigger_comment_id": rootID, "squad_id": squadID,
				"originator_user_id": testUserID, "accountable_user_id": testUserID,
				"session_id": "worker-replay-worker-session", "work_dir": "/work/worker-replay-worker",
			})
			first := claimWorkerReplyRun(t, runtimeID)
			if first == nil || first.ID != taskID {
				t.Fatal("leader run was not claimed")
			}
			var replyID string
			if mode == "unplanned" {
				replyID = dbfx.Comment(t, issueID, "An ordinary reply with no accepted dispatch", testutil.Cols{
					"parent_id": rootID, "author_type": "agent", "author_id": workerID, "source_task_id": workerTaskID,
				})
			} else {
				content := fmt.Sprintf("[@Squad](mention://squad/%s) The worker has results", squadID)
				if mode == "note" {
					content = fmt.Sprintf("/note [@Squad](mention://squad/%s) an informational update", squadID)
				}
				body := map[string]any{"content": content, "parent_id": rootID}
				if mode == "suppressed" {
					body["suppress_agent_ids"] = []string{leaderID}
				}
				req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", body), "id", issueID)
				req.Header.Set("X-Agent-ID", workerID)
				req.Header.Set("X-Task-ID", workerTaskID)
				req.Header.Set("X-Actor-Source", "task_token")
				if mode == "leader_reply" {
					req.Header.Set("X-Agent-ID", leaderID)
					req.Header.Set("X-Task-ID", taskID)
					req.Header.Set("X-Actor-Source", "task_token")
				}
				var response CommentResponse
				testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&response)
				replyID = response.ID
			}
			task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			comments, err := testHandler.Queries.ListReconcilableCommentsForIssueSince(ctx, db.ListReconcilableCommentsForIssueSinceParams{
				IssueID: task.IssueID, CommentThreadID: task.CommentThreadID, Since: task.CreatedAt,
				PlannedCommentIds: task.CoalescedCommentIds,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(comments, func(c db.Comment) bool { return c.ID == parseUUID(replyID) }) {
				t.Fatal("reply must reach the replay filter, not be excluded by the SQL thread/time window")
			}
			// A dispatched leader cannot atomically fold a later comment; the
			// explicit mention defers to completion reconcile instead.
			if slices.Contains(task.CoalescedCommentIds, parseUUID(replyID)) {
				t.Fatalf("%s acquired a creation-time obligation while the leader was dispatched", mode)
			}
			if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(taskID)); err != nil {
				t.Fatal(err)
			}
			completeWorkerReplyRun(t, taskID)
			queued := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID)
			if mode != "explicit_deferred" {
				if queued != 0 {
					t.Fatalf("%s must not create a completion-driven run, got %d", mode, queued)
				}
				return
			}
			if queued != 1 {
				t.Fatalf("explicit deferred reply must create exactly one successor, got %d", queued)
			}
			next := claimWorkerReplyRun(t, runtimeID)
			if next == nil || !next.IsLeaderTask || !slices.Contains(next.DeliveredCommentIDs, replyID) {
				t.Fatal("explicit deferred reply was not delivered in the leader role")
			}
			if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(next.ID)); err != nil {
				t.Fatal(err)
			}
			completeWorkerReplyRun(t, next.ID)
			if extra := claimWorkerReplyRun(t, runtimeID); extra != nil {
				t.Fatal("delivered explicit reply replayed again")
			}
		})
	}
}

// If completion commits while registration waits for the task row lock, the
// caller must see a miss and enqueue fresh, not attach an obligation to a run
// whose completion snapshot can no longer include it.
func TestWorkerReplyRegistrationLosesCompletionRace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runtimeID := dbfx.Runtime(t, "Worker registration race")
	agentID := dbfx.Agent(t, "Worker registration race", runtimeID)
	issueID := dbfx.Issue(t, "Worker registration race")
	rootID := dbfx.Comment(t, issueID, "Initial input")
	replyID := dbfx.Comment(t, issueID, "Worker result", testutil.Cols{"parent_id": rootID})
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "trigger_comment_id": rootID,
	})
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, taskID); err != nil {
		t.Fatal(err)
	}
	conn, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	done := make(chan struct{})
	var registrationErr error
	go func() {
		_, registrationErr = db.New(conn).RegisterPlannedCommentForActiveTask(ctx, db.RegisterPlannedCommentForActiveTaskParams{
			IssueID: parseUUID(issueID), AgentID: parseUUID(agentID), CommentID: parseUUID(replyID),
		})
		close(done)
	}()
	// Stop the registration query before returning its connection to the pool,
	// including assertion failures while it is waiting for the task row lock.
	defer func() { cancel(); <-done }()
	for {
		var blocked bool
		if err := testPool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("registration did not block on the completing row")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if !errors.Is(registrationErr, pgx.ErrNoRows) {
		t.Fatalf("registration after completion = %v, want no rows", registrationErr)
	}
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(task.CoalescedCommentIds, parseUUID(replyID)) {
		t.Fatal("completed run acquired an undeliverable obligation")
	}
}
