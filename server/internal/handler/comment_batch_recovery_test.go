package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type acceptedCommentBatch struct {
	issue, target, runtime, source, token, first, second, task string
}

func newAcceptedCommentBatch(t *testing.T) acceptedCommentBatch {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Fatal("PostgreSQL is required for comment batch recovery tests")
	}
	_, source, tokenA := managedDispatcherFixture(t, "batch-survivor-a")
	_, _, tokenB := managedDispatcherFixture(t, "batch-survivor-b")
	target, runtime := executableTarget(t, "batch-survivor-target")
	issue := dbfx.Issue(t, "Accepted comment batch")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
	code, first, body := intentComment(t, tokenA, issue, fmt.Sprintf("[@X](mention://agent/%s) first request", target), "")
	if code != http.StatusCreated {
		t.Fatalf("first comment = %d: %s", code, body)
	}
	task := latestTaskIDFor(t, issue, target)
	code, second, body := intentComment(t, tokenB, issue, fmt.Sprintf("[@X](mention://agent/%s) second request", target), first)
	if code != http.StatusCreated {
		t.Fatalf("second comment = %d: %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND trigger_comment_id=$3 AND $2::uuid=ANY(coalesced_comment_ids)`, task, first, second); n != 1 {
		t.Fatal("comments did not form one batch")
	}
	accepted := acceptedReturns(t, issue)
	if len(accepted) != 2 {
		t.Fatalf("batch accepted %d dispatcher returns, want 2", len(accepted))
	}
	requireTaskReturns(t, task, accepted...)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now(), delivered_comment_ids=ARRAY[$2::uuid,$3::uuid] WHERE id=$1`, task, first, second)
	return acceptedCommentBatch{issue, target, runtime, source, tokenB, first, second, task}
}

func (b acceptedCommentBatch) edit(t *testing.T, content string, want int) {
	t.Helper()
	w := serveAuthenticated(t, b.token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch,
		"/api/workspaces/"+testWorkspaceID+"/comments/"+b.second,
		map[string]any{"content": content}, map[string]string{"commentId": b.second}, nil)
	if w.Code != want {
		t.Fatalf("edit = %d, want %d: %s", w.Code, want, w.Body.String())
	}
}

func (b acceptedCommentBatch) liveCoverage(t *testing.T, comment string) int {
	t.Helper()
	return countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND issue_id=$2
		AND status NOT IN ('completed','failed','cancelled')
		AND (trigger_comment_id=$3 OR $3::uuid=ANY(coalesced_comment_ids) OR $3::uuid=ANY(delivered_comment_ids))`, b.target, b.issue, comment)
}

func consumeBatchRecovery(t *testing.T) {
	t.Helper()
	if _, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
}

func TestCommentBatchEditRecoversSurvivorsWithAcceptedReturns(t *testing.T) {
	b := newAcceptedCommentBatch(t)
	// The original sender need not remain active after its return was accepted.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, b.source)
	b.edit(t, fmt.Sprintf("[@X](mention://agent/%s) revised second request", b.target), http.StatusOK)
	if b.liveCoverage(t, b.first) != 1 || b.liveCoverage(t, b.second) != 1 {
		t.Fatalf("batch lost coverage: first=%d second=%d", b.liveCoverage(t, b.first), b.liveCoverage(t, b.second))
	}
	run := latestTaskIDFor(t, b.issue, b.target)
	if run == b.task {
		t.Fatal("cancelled run still owns the batch")
	}
	requireTaskReturns(t, run, acceptedReturns(t, b.issue)...)
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE issue_id=$1 AND status='settled' AND settled_task_id=$2`, b.issue, run); n != 2 {
		t.Fatalf("settled surviving obligations = %d", n)
	}
	consumeBatchRecovery(t)
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, b.issue); n != 2 {
		t.Fatalf("repeated recovery created %d runs", n)
	}
}

func TestCommentBatchDeleteRecoversOnlySurvivingIntent(t *testing.T) {
	b := newAcceptedCommentBatch(t)
	w := serveAuthenticated(t, b.token, http.HandlerFunc(testHandler.DeleteComment), http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/comments/"+b.second, nil, map[string]string{"commentId": b.second}, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	consumeBatchRecovery(t)
	if b.liveCoverage(t, b.first) != 1 || b.liveCoverage(t, b.second) != 0 {
		t.Fatalf("delete recovery: first=%d deleted=%d", b.liveCoverage(t, b.first), b.liveCoverage(t, b.second))
	}
	run := latestTaskIDFor(t, b.issue, b.target)
	for _, accepted := range acceptedReturns(t, b.issue) {
		if uuidToString(accepted.SourceTaskID) == b.source {
			requireTaskReturns(t, run, accepted)
		}
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1`, b.second); n != 0 {
		t.Fatalf("deleted intent kept %d obligations", n)
	}
}

func TestCommentBatchRecoverySurvivesEnqueueFailure(t *testing.T) {
	b := newAcceptedCommentBatch(t)
	drop := failCommentTaskInsertFor(t, b.target, "batch_retry")
	b.edit(t, fmt.Sprintf("[@X](mention://agent/%s) revised after failure", b.target), http.StatusOK)
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE issue_id=$1 AND status='pending'`, b.issue); n != 2 {
		t.Fatalf("accepted pending intents = %d, want 2", n)
	}
	if b.liveCoverage(t, b.first) != 0 || b.liveCoverage(t, b.second) != 0 {
		t.Fatal("injected failure unexpectedly created a covering run")
	}
	drop()
	dbfx.Exec(t, `UPDATE comment_dispatch_obligation SET next_attempt_at=now() WHERE issue_id=$1`, b.issue)
	consumeBatchRecovery(t)
	if b.liveCoverage(t, b.first) != 1 || b.liveCoverage(t, b.second) != 1 {
		t.Fatal("restarted consumer lost the accepted batch")
	}
	requireTaskReturns(t, latestTaskIDFor(t, b.issue, b.target), acceptedReturns(t, b.issue)...)
}

func TestCommentBatchRecoveryHonorsRevocation(t *testing.T) {
	b := newAcceptedCommentBatch(t)
	other := dbfx.User(t, "Revoked batch target owner", fmt.Sprintf("batch-revoked-%d@example.com", time.Now().UnixNano()))
	dbfx.Exec(t, `UPDATE agent SET permission_mode='private', owner_id=$2 WHERE id=$1`, b.target, other)
	dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, b.target)
	dbfx.Exec(t, `UPDATE agent_runtime SET owner_id=$2 WHERE id=$1`, b.runtime, other)
	b.edit(t, "withdrawn second request", http.StatusOK)
	consumeBatchRecovery(t)
	if b.liveCoverage(t, b.first) != 0 {
		t.Fatal("revoked target received surviving work")
	}
	var status, reason string
	if err := testPool.QueryRow(context.Background(), `SELECT status, last_error FROM comment_dispatch_obligation WHERE comment_id=$1`, b.first).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(reason, "invocation_not_allowed") {
		t.Fatalf("revocation = %s %q", status, reason)
	}
}

func TestCommentBatchRecoveryDoesNotReplaceTerminalRun(t *testing.T) {
	for _, status := range []string{"completed", "cancelled", "failed"} {
		t.Run(status, func(t *testing.T) {
			b := newAcceptedCommentBatch(t)
			if status == "cancelled" {
				w := serveAuthenticated(t, b.token, http.HandlerFunc(testHandler.CancelTask), http.MethodPost,
					"/api/issues/"+b.issue+"/tasks/"+b.task+"/cancel", nil, map[string]string{"id": b.issue, "taskId": b.task}, nil)
				if w.Code != http.StatusOK {
					t.Fatalf("user cancel = %d: %s", w.Code, w.Body.String())
				}
			} else {
				dbfx.Exec(t, `UPDATE agent_task_queue SET status=$2, completed_at=now() WHERE id=$1`, b.task, status)
			}
			b.edit(t, "withdrawn second request", http.StatusOK)
			consumeBatchRecovery(t)
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, b.issue); n != 1 {
				t.Fatalf("terminal batch spawned %d runs", n)
			}
			coverage, handled := testHandler.replayCommentDispatchObligation(context.Background(), parseUUID(b.first), parseUUID(b.target))
			if !handled || !coverage.settled || uuidToString(coverage.settledTask) != b.task {
				t.Fatalf("delivered terminal receipt = %+v, handled=%v", coverage, handled)
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, b.issue); n != 1 {
				t.Fatalf("receipt replay spawned %d runs", n)
			}
		})
	}
}

func TestCommentBatchReopenFailureRollsBackMutation(t *testing.T) {
	for _, mutation := range []string{"edit", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			b := newAcceptedCommentBatch(t)
			fn := "fail_batch_reopen_" + strings.ReplaceAll(b.issue, "-", "_")
			dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.comment_id='%s' AND NEW.status='pending' THEN RAISE EXCEPTION 'injected survivor recovery failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE UPDATE ON comment_dispatch_obligation FOR EACH ROW EXECUTE FUNCTION %s();`, fn, b.first, fn, fn))
			t.Cleanup(func() {
				dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON comment_dispatch_obligation; DROP FUNCTION IF EXISTS %s();`, fn, fn))
			})
			if mutation == "edit" {
				b.edit(t, "changed text must roll back", http.StatusInternalServerError)
			} else {
				w := serveAuthenticated(t, b.token, http.HandlerFunc(testHandler.DeleteComment), http.MethodDelete,
					"/api/workspaces/"+testWorkspaceID+"/comments/"+b.second, nil, map[string]string{"commentId": b.second}, nil)
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
				}
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='running'`, b.task); n != 1 {
				t.Fatal("failed reopen committed batch cancellation")
			}
			if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE issue_id=$1 AND status='settled' AND settled_task_id=$2`, b.issue, b.task); n != 2 {
				t.Fatal("failed reopen changed accepted obligation coverage")
			}
			if n := countValue(t, `SELECT count(*) FROM comment WHERE id=$1 AND content='changed text must roll back'`, b.second); n != 0 {
				t.Fatal("failed reopen committed edited content")
			}
			if n := countValue(t, `SELECT count(*) FROM comment WHERE id=$1 AND deleted_at IS NULL`, b.second); n != 1 {
				t.Fatal("failed reopen deleted the changed comment")
			}
			requireTaskReturns(t, b.task, acceptedReturns(t, b.issue)...)
		})
	}
}

func TestCommentBatchDeleteFailureKeepsOriginalCoverage(t *testing.T) {
	b := newAcceptedCommentBatch(t)
	fn := "fail_batch_delete_" + strings.ReplaceAll(b.issue, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF OLD.id='%s' THEN RAISE EXCEPTION 'injected comment delete failure'; END IF; RETURN OLD; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE DELETE ON comment FOR EACH ROW EXECUTE FUNCTION %s();`, fn, b.second, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON comment; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	// Compensation cannot enqueue; a restarted consumer must not be needed to
	// undo a deletion that never committed in the first place.
	drop := failCommentTaskInsertFor(t, b.target, "delete_rollback")
	w := serveAuthenticated(t, b.token, http.HandlerFunc(testHandler.DeleteComment), http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/comments/"+b.second, nil, map[string]string{"commentId": b.second}, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	drop()
	consumeBatchRecovery(t)
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, b.issue); n != 1 {
		t.Fatalf("failed deletion replaced the original batch with %d runs", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='running'`, b.task); n != 1 {
		t.Fatal("failed deletion committed batch cancellation")
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE issue_id=$1 AND status='settled' AND settled_task_id=$2`, b.issue, b.task); n != 2 {
		t.Fatalf("failed deletion lost complete accepted coverage: %d", n)
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND deleted_at IS NULL`, b.issue); n != 2 {
		t.Fatalf("failed deletion changed comment rows: %d", n)
	}
	requireTaskReturns(t, b.task, acceptedReturns(t, b.issue)...)
}
