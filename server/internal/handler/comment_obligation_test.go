package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// pendingCommentObligation creates B's agent-authored comment mentioning X whose
// in-transaction enqueue fails, leaving exactly one pending obligation.
func pendingCommentObligation(t *testing.T, tag, parentID, issueID, tokB, x string) string {
	t.Helper()
	drop := failCommentTaskInsertFor(t, x, tag)
	code, cID, body := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) B request %s %d", x, tag, time.Now().UnixNano()), parentID)
	drop()
	if code != http.StatusCreated {
		t.Fatalf("B comment = %d: %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='pending'`, cID); n != 1 {
		t.Fatalf("pending obligations = %d, want 1", n)
	}
	return cID
}

// gateCommentTaskInsert blocks every agent_task_queue insert for agentID on an
// advisory lock held by the returned lock.
func gateCommentTaskInsert(t *testing.T, agentID, tag string) *advisoryLock {
	t.Helper()
	key := int64(870000 + time.Now().UnixNano()%100000)
	fn := "obligation_gate_" + tag + "_" + strings.ReplaceAll(agentID, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.agent_id = '%s' THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, fn, agentID, key, fn, fn))
	lock := holdAdvisoryLock(t, key)
	t.Cleanup(func() {
		lock.release()
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	return lock
}

// waitCommentMutationLock waits until some backend waits on a transaction (row) lock.
func waitCommentMutationLock(t *testing.T, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n := countValue(t, `SELECT count(*) FROM pg_locks WHERE locktype='transactionid' AND NOT granted`)
		if n > 0 {
			return
		}
		select {
		case w := <-done:
			t.Fatalf("mutation finished without waiting on the consumer: %d %s", w.Code, w.Body.String())
		default:
		}

	}
	t.Fatal("mutation never waited on the consumer's row lock")
}

func consumeCommentsAsync(t *testing.T) <-chan CommentDispatchObligationStats {
	out := make(chan CommentDispatchObligationStats, 1)
	go func() {
		stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
		if err != nil {
			t.Errorf("consume: %v", err)
		}
		out <- stats
	}()
	return out
}

// An edit withdrawing @X must cancel the run created by an in-flight consumer.
func TestCommentObligationEditCancelsConsumerRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, tokB := managedDispatcherFixture(t, "obligation-edit-b")
	x, _ := executableTarget(t, "obligation-edit-x")
	issueID := dbfx.Issue(t, "Comment obligation edit interleave")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	cID := pendingCommentObligation(t, "edit", "", issueID, tokB, x)

	lock := gateCommentTaskInsert(t, x, "edit")
	consumed := consumeCommentsAsync(t)
	waitForBackendBlockedBy(t, lock.holderPID(t))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveAuthenticated(t, tokB, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch,
			"/api/workspaces/"+testWorkspaceID+"/comments/"+cID, map[string]any{"content": "withdrawn: no request any more"}, map[string]string{"commentId": cID}, nil)
	}()
	waitCommentMutationLock(t, done)
	lock.release()
	stats := <-consumed
	w := waitForResponse(t, done)
	t.Logf("consumer=%+v edit=%d", stats, w.Code)

	live := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2 AND status IN ('queued','dispatched','running')`, x, cID)
	obl := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1`, cID)
	t.Logf("live X runs for edited comment=%d obligations left=%d", live, obl)
	if w.Code != http.StatusOK || live != 0 || obl != 0 {
		t.Fatalf("edit did not withdraw the old intent: status=%d live=%d obligations=%d", w.Code, live, obl)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2 AND status='cancelled'`, x, cID); n != 1 {
		t.Fatalf("cancelled old run count = %d, want 1", n)
	}
}

// Deletion must cancel the run created by an in-flight consumer.
func TestCommentObligationDeleteCancelsConsumerRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, tokB := managedDispatcherFixture(t, "obligation-del-b")
	x, _ := executableTarget(t, "obligation-del-x")
	issueID := dbfx.Issue(t, "Comment obligation delete interleave")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	cID := pendingCommentObligation(t, "del", "", issueID, tokB, x)

	lock := gateCommentTaskInsert(t, x, "del")
	consumed := consumeCommentsAsync(t)
	waitForBackendBlockedBy(t, lock.holderPID(t))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveAuthenticated(t, tokB, http.HandlerFunc(testHandler.DeleteComment), http.MethodDelete,
			"/api/workspaces/"+testWorkspaceID+"/comments/"+cID, nil, map[string]string{"commentId": cID}, nil)
	}()
	waitCommentMutationLock(t, done)
	lock.release()
	stats := <-consumed
	w := waitForResponse(t, done)
	t.Logf("consumer=%+v delete=%d", stats, w.Code)

	live := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2 AND status IN ('queued','dispatched','running')`, x, cID)
	exists := countValue(t, `SELECT count(*) FROM comment WHERE id=$1`, cID)
	t.Logf("live X runs for deleted comment=%d comment rows=%d", live, exists)
	if w.Code >= 300 || live != 0 || exists != 0 {
		t.Fatalf("delete did not withdraw the old intent: status=%d live=%d comments=%d", w.Code, live, exists)
	}
}

// Revocation: after acceptance X becomes private to another owner. Replay must
// not bypass the current invocation gate that completion replay rechecks.
func TestCommentObligationConsumerChecksCurrentInvocationPermission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, tokB := managedDispatcherFixture(t, "obligation-perm-b")
	x, runtimeX := executableTarget(t, "obligation-perm-x")
	issueID := dbfx.Issue(t, "Comment obligation revoked permission")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	cID := pendingCommentObligation(t, "perm", "", issueID, tokB, x)

	other := dbfx.User(t, "Other target owner", fmt.Sprintf("obligation-%d@example.com", time.Now().UnixNano()))
	dbfx.Exec(t, `UPDATE agent SET permission_mode='private', owner_id=$2 WHERE id=$1`, x, other)
	dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, x)
	dbfx.Exec(t, `UPDATE agent_runtime SET owner_id=$2 WHERE id=$1`, runtimeX, other)

	_, _, ctrl := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) control after revoke", x), "")
	if i := strings.Index(ctrl, "trigger_outcomes"); i >= 0 {
		t.Logf("control fresh mention: %s", ctrl[i:])
	}
	if !strings.Contains(ctrl, "invocation_not_allowed") {
		t.Fatalf("fresh mention did not observe revocation: %s", ctrl)
	}
	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
	t.Logf("consumer=%+v err=%v", stats, err)
	runs := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, cID)
	var status, lastErr string
	_ = testPool.QueryRow(context.Background(), `SELECT status, coalesce(last_error,'') FROM comment_dispatch_obligation WHERE comment_id=$1`, cID).Scan(&status, &lastErr)
	t.Logf("runs for revoked target=%d obligation=%s %q", runs, status, lastErr)
	if runs != 0 || status != "failed" || !strings.Contains(lastErr, "invocation_not_allowed") {
		t.Errorf("revocation must fail diagnostically without a run: runs=%d status=%s error=%s", runs, status, lastErr)
	}
}

// Completion replay settles the obligation on a success-shaped "deferred"
// that is only a prediction: an active dispatched run newer than the comment
// neither carries it nor will replay it.
func TestCommentObligationReplayRecordsDurableCoverage(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, tokA := managedDispatcherFixture(t, "obligation-def-a")
	_, _, tokB := managedDispatcherFixture(t, "obligation-def-b")
	x, runtimeX := executableTarget(t, "obligation-def-x")
	issueID := dbfx.Issue(t, "Comment obligation replay deferred settle")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)
	cID := pendingCommentObligation(t, "def", c1, issueID, tokB, x)

	// A newer run in the same thread was claimed (dispatched) before run1
	// completes. It neither has cID as trigger nor in its planned ids.

	d := dbfx.Task(t, x, testutil.Cols{
		"issue_id":          issueID,
		"runtime_id":        runtimeX,
		"status":            "dispatched",
		"comment_thread_id": c1,
	})

	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run1))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)

	var status string
	_ = testPool.QueryRow(context.Background(), `SELECT status FROM comment_dispatch_obligation WHERE comment_id=$1`, cID).Scan(&status)
	carried := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND (trigger_comment_id=$2 OR $2=ANY(coalesced_comment_ids))`, x, cID)
	newerThanD := countValue(t, `SELECT count(*) FROM comment c, agent_task_queue q WHERE c.id=$1 AND q.id=$2 AND c.created_at > q.created_at`, cID, d)
	t.Logf("obligation=%s runs carrying comment=%d comment newer than D=%v", status, carried, newerThanD == 1)
	if status == "settled" && carried == 0 && newerThanD == 0 {
		t.Errorf("obligation settled with no run carrying the comment, and D's completion replay window cannot see it")
	}
}

// A settlement failure must roll back the replayed run, leaving one pending
// intent that the restarted consumer can execute exactly once.
func TestCommentObligationReplaySettlementFailureRollsBackRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, tokA := managedDispatcherFixture(t, "obligation-dup-a")
	_, _, tokB := managedDispatcherFixture(t, "obligation-dup-b")
	x, _ := executableTarget(t, "obligation-dup-x")
	issueID := dbfx.Issue(t, "Comment obligation lost settle")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)
	cID := pendingCommentObligation(t, "dup", c1, issueID, tokB, x)

	fn := "obligation_settle_fail_" + strings.ReplaceAll(x, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.status = 'settled' AND NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected settle failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE UPDATE ON comment_dispatch_obligation FOR EACH ROW EXECUTE FUNCTION %s();`, fn, x, fn, fn))
	dropped := false
	drop := func() {
		if !dropped {
			dropped = true
			dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON comment_dispatch_obligation; DROP FUNCTION IF EXISTS %s();`, fn, fn))
		}
	}
	t.Cleanup(drop)

	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run1))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)
	drop()
	first := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, cID)
	t.Logf("runs after replay=%d", first)
	if first != 0 {
		t.Fatalf("settlement failure left %d runs committed, want atomic rollback", first)
	}
	dbfx.Exec(t, `UPDATE comment_dispatch_obligation SET next_attempt_at=now() WHERE comment_id=$1`, cID)

	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
	t.Logf("consumer=%+v err=%v", stats, err)
	total := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, cID)
	t.Logf("runs executing B's comment=%d", total)
	if total != 1 {
		t.Fatalf("recovery produced %d runs, want 1", total)
	}
	run := latestTaskIDFor(t, issueID, x)
	returns := taskReturnIDs(t, run)
	if len(returns) != 1 {
		t.Fatalf("recovered run has %d returns, want only B", len(returns))
	}
	if n := countValue(t, `SELECT count(*) FROM comment_delegation_return c JOIN agent_task_delegation_return r USING(delegation_return_id) WHERE c.comment_id=$1 AND r.task_id=$2`, cID, run); n != 1 {
		t.Fatal("recovery lost accepted return")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run, cID)
	if _, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, cID); n != 1 {
		t.Fatalf("completed recovery repeated: %d runs", n)
	}
}

// Concurrent consumers on one pending obligation produce exactly one run.
func TestCommentObligationConcurrentConsumersSingleRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, tokB := managedDispatcherFixture(t, "obligation-cc-b")
	x, _ := executableTarget(t, "obligation-cc-x")
	issueID := dbfx.Issue(t, "Comment obligation concurrent consumers")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	cID := pendingCommentObligation(t, "cc", "", issueID, tokB, x)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
		}()
	}
	wg.Wait()
	runs := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, cID)
	t.Logf("runs=%d", runs)
	if runs != 1 {
		t.Errorf("concurrent consumers produced %d runs, want 1", runs)
	}
}

func failCommentTaskInsertFor(t *testing.T, agentID, tag string) func() {
	t.Helper()
	fn := "obligation_fail_" + tag + "_" + strings.ReplaceAll(agentID, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected enqueue failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, fn, agentID, fn, fn))
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)
	return drop
}

func TestCommentObligationCompletedRunDoesNotCoverNewEdit(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, token := managedDispatcherFixture(t, "obligation-edit-generation")
	x, _ := executableTarget(t, "obligation-edit-generation-x")
	issueID := dbfx.Issue(t, "Comment obligation edited generation")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	code, commentID, body := intentComment(t, token, issueID, fmt.Sprintf("[@X](mention://agent/%s) original request", x), "")
	if code != http.StatusCreated {
		t.Fatalf("comment = %d: %s", code, body)
	}
	first := latestTaskIDFor(t, issueID, x)
	var oldObligation string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM comment_dispatch_obligation WHERE comment_id=$1`, commentID).Scan(&oldObligation); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, first, commentID)
	drop := failCommentTaskInsertFor(t, x, "editgeneration")
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch,
		"/api/comments/"+commentID, map[string]any{"content": fmt.Sprintf("[@X](mention://agent/%s) new request after completed work", x)}, map[string]string{"commentId": commentID}, nil)
	drop()
	if w.Code != http.StatusOK {
		t.Fatalf("edit = %d: %s", w.Code, w.Body.String())
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND id <> $2 AND status='pending' AND settled_task_id IS NULL`, commentID, oldObligation); n != 1 {
		t.Fatalf("new pending generation = %d", n)
	}
	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
	if err != nil || stats.Covered != 1 {
		t.Fatalf("consumer = %+v, %v", stats, err)
	}
	second := latestTaskIDFor(t, issueID, x)
	if second == first {
		t.Fatal("old receipt swallowed the new edit")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, commentID); n != 2 {
		t.Fatalf("runs = %d, want 2 generations", n)
	}
	requireTaskReturns(t, second, acceptedReturns(t, issueID)...)
}

func TestCommentObligationExistingReceiptPreservesReturns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, token := managedDispatcherFixture(t, "obligation-receipt")
	x, _ := executableTarget(t, "obligation-receipt-x")
	issueID := dbfx.Issue(t, "Comment obligation receipt")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	code, commentID, body := intentComment(t, token, issueID, fmt.Sprintf("[@X](mention://agent/%s) request", x), "")
	if code != http.StatusCreated {
		t.Fatalf("comment = %d: %s", code, body)
	}
	run := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run, commentID)
	dbfx.Exec(t, `UPDATE comment_dispatch_obligation SET status='pending' WHERE comment_id=$1`, commentID)
	dbfx.Exec(t, `DELETE FROM agent_task_delegation_return WHERE task_id=$1`, run)
	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
	if err != nil || stats.Covered != 1 {
		t.Fatalf("consumer = %+v, %v", stats, err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, commentID); n != 1 {
		t.Fatalf("receipt produced %d runs", n)
	}
	requireTaskReturns(t, run, acceptedReturns(t, issueID)...)
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='settled' AND settled_task_id=$2`, commentID, run); n != 1 {
		t.Fatalf("settled receipt = %d", n)
	}
}

func TestCommentObligationUnavailableAuthorFailsClosed(t *testing.T) {
	for _, transition := range []string{"archived", "missing"} {
		t.Run(transition, func(t *testing.T) {
			if testHandler == nil || testPool == nil {
				t.Skip("database not available")
			}
			author, _, token := managedDispatcherFixture(t, "obligation-author-"+transition)
			x, _ := executableTarget(t, "obligation-author-x-"+transition)
			issueID := dbfx.Issue(t, "Comment obligation unavailable author")
			t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
			commentID := pendingCommentObligation(t, "author"+transition, "", issueID, token, x)
			if transition == "archived" {
				dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, author)
			} else {
				dbfx.Exec(t, `DELETE FROM agent WHERE id=$1`, author)
			}
			stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
			if err != nil || stats.Failed != 1 {
				t.Fatalf("consumer = %+v, %v", stats, err)
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, commentID); n != 0 {
				t.Fatalf("unavailable author produced %d runs", n)
			}
		})
	}
}

func TestCommentObligationSquadRechecksPermissionAndIdentity(t *testing.T) {
	for _, transition := range []string{"revoked", "new_leader", "closed_source"} {
		t.Run(transition, func(t *testing.T) {
			if testHandler == nil || testPool == nil {
				t.Skip("database not available")
			}
			_, source, token := managedDispatcherFixture(t, "obligation-squad-"+transition)
			x, runtimeID := executableTarget(t, "obligation-squad-x-"+transition)
			squad := dbfx.Squad(t, "Obligation squad", x)
			issueID := dbfx.Issue(t, "Comment obligation squad")
			t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
			drop := failCommentTaskInsertFor(t, x, "squad"+transition)
			code, commentID, body := intentComment(t, token, issueID, fmt.Sprintf("[@Squad](mention://squad/%s) request", squad), "")
			drop()
			if code != http.StatusCreated {
				t.Fatalf("comment = %d: %s", code, body)
			}
			if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='pending' AND squad_id=$2`, commentID, squad); n != 1 {
				t.Fatalf("pending squad obligation = %d", n)
			}
			switch transition {
			case "revoked":
				other := dbfx.User(t, "other", fmt.Sprintf("squad-%d@example.com", time.Now().UnixNano()))
				dbfx.Exec(t, `UPDATE agent SET permission_mode='private', owner_id=$2 WHERE id=$1`, x, other)
				dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, x)
				dbfx.Exec(t, `UPDATE agent_runtime SET owner_id=$2 WHERE id=$1`, runtimeID, other)
			case "new_leader":
				y, _ := executableTarget(t, "obligation-squad-y")
				dbfx.Exec(t, `UPDATE squad SET leader_id=$2 WHERE id=$1`, squad, y)
			case "closed_source":
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, source)
			}
			stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
			if err != nil {
				t.Fatal(err)
			}
			runs := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID)
			if transition == "closed_source" {
				if stats.Covered != 1 || runs != 1 {
					t.Fatalf("accepted intent lost after source completion: %+v runs=%d", stats, runs)
				}
				run := latestTaskIDFor(t, issueID, x)
				requireTaskReturns(t, run, acceptedReturns(t, issueID)...)
				if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND squad_id=$2 AND is_leader_task`, run, squad); n != 1 {
					t.Fatal("squad role lost")
				}
			} else if stats.Failed != 1 || runs != 0 {
				t.Fatalf("revoked squad executed: %+v runs=%d", stats, runs)
			}
		})
	}
}

func TestCommentObligationMissingCommentFailsWithoutDispatch(t *testing.T) {
	for _, transition := range []string{"missing", "tombstone"} {
		t.Run(transition, func(t *testing.T) {
			if testHandler == nil || testPool == nil {
				t.Skip("database not available")
			}
			_, _, token := managedDispatcherFixture(t, "obligation-comment-"+transition)
			x, _ := executableTarget(t, "obligation-comment-x-"+transition)
			issueID := dbfx.Issue(t, "Comment obligation deleted input")
			t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
			commentID := pendingCommentObligation(t, "comment"+transition, "", issueID, token, x)
			if transition == "missing" {
				dbfx.Exec(t, `DELETE FROM comment WHERE id=$1`, commentID)
			} else {
				dbfx.Exec(t, `UPDATE comment SET deleted_at=now(), content='' WHERE id=$1`, commentID)
			}
			stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
			if err != nil || stats.Failed != 1 {
				t.Fatalf("consumer = %+v, %v", stats, err)
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, x, commentID); n != 0 {
				t.Fatalf("missing comment produced %d runs", n)
			}
			stats, err = restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
			if err != nil || stats.Scanned != 0 {
				t.Fatalf("failed intent retried: %+v, %v", stats, err)
			}
		})
	}
}

func TestCommentObligationUndeliveredSettledRunKeepsRetry(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, token := managedDispatcherFixture(t, "obligation-undelivered")
	x, _ := executableTarget(t, "obligation-undelivered-x")
	issueID := dbfx.Issue(t, "Comment obligation undelivered run")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	code, commentID, body := intentComment(t, token, issueID, fmt.Sprintf("[@X](mention://agent/%s) request", x), "")
	if code != http.StatusCreated {
		t.Fatalf("comment = %d: %s", code, body)
	}
	run := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), delivered_comment_ids='{}' WHERE id=$1`, run)
	drop := failCommentTaskInsertFor(t, x, "undelivered")
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)
	drop()
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='pending' AND attempts>0`, commentID); n != 1 {
		t.Fatalf("undelivered replay left %d retry obligations", n)
	}
	dbfx.Exec(t, `UPDATE comment_dispatch_obligation SET next_attempt_at=now() WHERE comment_id=$1`, commentID)
	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50)
	if err != nil || stats.Covered != 1 {
		t.Fatalf("consumer = %+v, %v", stats, err)
	}
	follow := latestTaskIDFor(t, issueID, x)
	if follow == run {
		t.Fatal("undelivered run did not produce follow-up")
	}
	requireTaskReturns(t, follow, acceptedReturns(t, issueID)...)
}
