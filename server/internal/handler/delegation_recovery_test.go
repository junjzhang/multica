package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// holdAdvisoryLock takes a session advisory lock from a dedicated backend. A
// trigger can then block inside the write under test until the test has applied
// the state transition, which is a real barrier rather than a sleep.
type advisoryLock struct {
	conn *pgxpool.Conn
	key  int64
}

func holdAdvisoryLock(t *testing.T, key int64) *advisoryLock {
	t.Helper()
	conn, err := testPool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		t.Fatalf("take advisory lock: %v", err)
	}
	lock := &advisoryLock{conn: conn, key: key}
	t.Cleanup(lock.release)
	return lock
}

func (l *advisoryLock) release() {
	if l.conn == nil {
		return
	}
	_, _ = l.conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", l.key)
	l.conn.Release()
	l.conn = nil
}

func (l *advisoryLock) holderPID(t *testing.T) int32 {
	t.Helper()
	var pid int32
	if err := l.conn.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read lock holder pid: %v", err)
	}
	return pid
}

func waitForResponse(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(15 * time.Second):
		t.Fatal("request did not finish")
		return nil
	}
}

// restartedHandler models a process restart: no request-scoped state survives,
// only the durable database rows and a freshly constructed query handle.
func restartedHandler() *Handler {
	h := *testHandler
	h.Queries = db.New(testPool)
	return &h
}

// The assignment fallback used to read only queued/dispatched occupants while
// the unique index also covers channel-media deferred rows. The deferred
// occupant then looked "gone": the issue was reassigned, no run was created,
// and the accepted return was silently dropped.
func TestAssignmentLinksReturnIntoDeferredMediaOccupant(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, _, token := managedDispatcherFixture(t, "assign-deferred")
	target, runtimeID := executableTarget(t, "assign-deferred-target")
	issueID := dbfx.Issue(t, "Assignment deferred occupant")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	occupant := dbfx.Task(t, target, testutil.Cols{
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "deferred",
		"context":    testutil.Raw(`'{"channel_issue_media_pending":"true"}'::jsonb`),
	})

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": target}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("assignment = %d: %s", w.Code, w.Body.String())
	}
	if got := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, target); got != 1 {
		t.Fatalf("assignment created %d runs for the occupied slot, want the deferred occupant only", got)
	}
	accepted, ok := acceptedReturn(t, issueID)
	if !ok || uuidToString(accepted.DispatcherAgentID) != dispatcher {
		t.Fatalf("accepted return missing: %+v", acceptedReturns(t, issueID))
	}
	requireTaskReturns(t, occupant, accepted)
	if n := countValue(t, `SELECT count(*) FROM issue_assignment_delegation WHERE issue_id=$1 AND task_id=$2`, issueID, occupant); n != 1 {
		t.Fatalf("assignment intent did not point at the deferred occupant")
	}
}

// A conflict observed before the occupant started must not turn into
// inserted=false, err=nil. The retry re-observes the occupant: once it is
// running (or gone), a fresh run is inserted and carries this assignment's
// accepted return.
func TestAssignmentConflictReobservesOccupantState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transition string
	}{
		{name: "running"},
		{name: "vanished", transition: `DELETE FROM agent_task_queue WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if testHandler == nil || testPool == nil {
				t.Skip("database not available")
			}
			dispatcher, _, token := managedDispatcherFixture(t, "assign-barrier-"+tc.name)
			target, runtimeID := executableTarget(t, "assign-barrier-target-"+tc.name)
			issueID := dbfx.Issue(t, "Assignment conflict barrier "+tc.name)
			t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

			// The occupant is already in the state the retry must re-observe:
			// running is still an active run, so the assignment must insert a
			// fresh run rather than report a no-op; vanished has no occupant left.
			occupant := dbfx.Task(t, target, testutil.Cols{
				"issue_id":   issueID,
				"runtime_id": runtimeID,
				"status":     "running",
			})
			if tc.name == "vanished" {
				// Keep it until the request is inside the trigger so the conflict
				// is observed under the barrier, then remove it before the retry.
			}

			suffix := fmt.Sprintf("%s_%d", strings.ReplaceAll(tc.name, "-", "_"), time.Now().UnixNano())
			key := int64(861000 + time.Now().UnixNano()%1000000)
			seq := "work86_assign_conflict_seq_" + suffix
			fn := "work86_assign_conflict_fn_" + suffix
			dbfx.Exec(t, fmt.Sprintf(`DROP SEQUENCE IF EXISTS %s; CREATE SEQUENCE %s START 1;`, seq, seq))
			dbfx.Exec(t, fmt.Sprintf(`
				CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
				BEGIN
					IF NEW.agent_id = '%s' THEN
						PERFORM pg_advisory_xact_lock(%d);
						IF nextval('%s') = 1 THEN
							RAISE EXCEPTION USING ERRCODE = 'unique_violation',
								CONSTRAINT = 'idx_one_pending_task_per_issue_agent_thread';
						END IF;
					END IF;
					RETURN NEW;
				END $$ LANGUAGE plpgsql;
				CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue
				FOR EACH ROW EXECUTE FUNCTION %s();`, fn, target, key, seq, fn, fn))
			t.Cleanup(func() {
				dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s(); DROP SEQUENCE IF EXISTS %s;`, fn, fn, seq))
			})

			lock := holdAdvisoryLock(t, key)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
					"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": target}, map[string]string{"id": issueID}, nil)
			}()
			waitForBackendBlockedBy(t, lock.holderPID(t))

			if tc.transition != "" {
				dbfx.Exec(t, tc.transition, occupant)
			}
			lock.release()

			w := waitForResponse(t, done)
			if w.Code != http.StatusOK {
				t.Fatalf("assignment = %d: %s", w.Code, w.Body.String())
			}
			accepted, ok := acceptedReturn(t, issueID)
			if !ok || uuidToString(accepted.DispatcherAgentID) != dispatcher {
				t.Fatalf("accepted return missing: %+v", acceptedReturns(t, issueID))
			}
			wantTasks := 2
			if tc.name == "vanished" {
				wantTasks = 1
			}
			if got := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, target); got != wantTasks {
				t.Fatalf("runs after assignment = %d, want %d", got, wantTasks)
			}
			runID := latestTaskIDFor(t, issueID, target)
			if runID == "" || runID == occupant {
				t.Fatalf("assignment did not insert a fresh run: latest=%s occupant=%s", runID, occupant)
			}
			requireTaskReturns(t, runID, accepted)
			if n := countValue(t, `SELECT count(*) FROM issue_assignment_delegation WHERE issue_id=$1 AND task_id=$2`, issueID, runID); n != 1 {
				t.Fatalf("assignment intent did not point at the retried run")
			}
		})
	}
}

// A comment accepted while its target run is about to complete keeps a durable
// obligation. The comment commits after the run completed and after its own
// enqueue failed; with no further run completion and no edit, only the
// obligation sweeper can create the follow-up, and that run carries exactly
// this comment's return.
func TestCompletedTargetCommentRecoveredByDurableObligation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	a, _, tokA := managedDispatcherFixture(t, "recover-order-a")
	b, _, tokB := managedDispatcherFixture(t, "recover-order-b")
	x, _ := executableTarget(t, "recover-order-x")
	_ = a
	issueID := dbfx.Issue(t, "Completed target durable recovery")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)

	key := int64(862000 + time.Now().UnixNano()%1000)
	fn := "work86_comment_barrier_" + strings.ReplaceAll(x, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON comment FOR EACH ROW EXECUTE FUNCTION %s();`, fn, key, fn, fn))
	lock := holdAdvisoryLock(t, key)
	t.Cleanup(func() {
		lock.release()
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON comment; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})

	failFn := "work86_comment_fail_" + strings.ReplaceAll(x, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected post-insert enqueue failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, failFn, x, failFn, failFn))
	dropFail := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, failFn, failFn))
	}
	t.Cleanup(dropFail)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveAuthenticated(t, tokB, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
			"/api/issues/"+issueID+"/comments", map[string]any{"content": fmt.Sprintf("[@X](mention://agent/%s) B request %d", x, time.Now().UnixNano()), "parent_id": c1}, map[string]string{"id": issueID}, nil)
	}()
	waitForBackendBlockedBy(t, lock.holderPID(t))

	// The target completes while B's comment is still uncommitted. Its
	// reconciliation cannot see B's comment, so no follow-up is scheduled.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run1))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)
	if got := latestTaskIDFor(t, issueID, x); got != run1 {
		t.Fatalf("reconcile saw the uncommitted comment and created %s", got)
	}
	lock.release()

	w := waitForResponse(t, done)
	if w.Code != http.StatusCreated {
		t.Fatalf("comment = %d, want the comment accepted with a durable obligation: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	dropFail()

	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND agent_id=$2 AND status='pending'`, created.ID, x); n != 1 {
		t.Fatalf("pending obligation rows = %d, want 1", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, x); n != 1 {
		t.Fatalf("failed post-insert enqueue left %d runs, want only the completed source", n)
	}

	// No new event, no edit, no run completion: the durable consumer is the
	// only possible progressor.
	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 10)
	if err != nil {
		t.Fatalf("consume obligation: %v", err)
	}
	if stats.Covered != 1 {
		t.Fatalf("consumer covered %d obligations, want 1", stats.Covered)
	}
	follow := latestTaskIDFor(t, issueID, x)
	if follow == "" || follow == run1 {
		t.Fatalf("consumer created no follow-up run (latest=%s)", follow)
	}
	var trigger string
	_ = testPool.QueryRow(context.Background(), `SELECT coalesce(trigger_comment_id::text,'') FROM agent_task_queue WHERE id=$1`, follow).Scan(&trigger)
	if trigger != created.ID {
		t.Fatalf("follow-up trigger = %s, want B comment %s", trigger, created.ID)
	}
	var bReturn string
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == b {
			bReturn = uuidToString(row.ID)
		}
	}
	if got := returnIDs(taskReturnIDs(t, follow)); len(got) != 1 || !got[bReturn] {
		t.Fatalf("follow-up returns = %v, want exactly B's %s", got, bReturn)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='settled'`, created.ID); n != 1 {
		t.Fatalf("settled obligation rows = %d, want 1", n)
	}
}

// A no-task_id rerun follows the current assignment intent's run. B's
// independent comment run on the same worker and issue must never be inherited;
// when no assignment intent exists but the issue has accepted returns, the
// request is refused instead of guessing the latest run.
func TestDefaultRerunUsesAssignmentIntentNotLatestRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	a, _, tokA := managedDispatcherFixture(t, "rerun-intent-a")
	b, _, tokB := managedDispatcherFixture(t, "rerun-intent-b")
	x, _ := executableTarget(t, "rerun-intent-x")
	issueID := dbfx.Issue(t, "Rerun assignment intent")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	// A assigns X: run1, assignment intent, A's accepted return.
	assign := serveAuthenticated(t, tokA, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": x}, map[string]string{"id": issueID}, nil)
	if assign.Code != http.StatusOK {
		t.Fatalf("assignment = %d: %s", assign.Code, assign.Body.String())
	}
	run1 := latestTaskIDFor(t, issueID, x)
	var aReturn db.IssueDelegationReturn
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == a {
			aReturn = row
		}
	}
	requireTaskReturns(t, run1, aReturn)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)

	// B independently mentions X in another thread: a new run carrying only B.
	_, c2, _ := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) B independent %d", x, time.Now().UnixNano()), "")
	run2 := latestTaskIDFor(t, issueID, x)
	if run2 == run1 {
		t.Fatal("B's independent comment coalesced into A's completed run")
	}
	var bReturn db.IssueDelegationReturn
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == b {
			bReturn = row
		}
	}
	if got := returnIDs(taskReturnIDs(t, run2)); len(got) != 1 || !got[uuidToString(bReturn.ID)] {
		t.Fatalf("B's independent run returns = %v, want only B", got)
	}
	_ = c2

	rerun := serveAuthenticated(t, tokA, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost,
		"/api/issues/"+issueID+"/rerun", map[string]any{}, map[string]string{"id": issueID}, nil)
	if rerun.Code != http.StatusAccepted {
		t.Fatalf("rerun = %d: %s", rerun.Code, rerun.Body.String())
	}
	run3 := latestTaskIDFor(t, issueID, x)
	if run3 == run2 || run3 == run1 {
		t.Fatalf("rerun did not create a new run (run1=%s run2=%s run3=%s)", run1, run2, run3)
	}
	if got := returnIDs(taskReturnIDs(t, run3)); len(got) != 1 || !got[uuidToString(aReturn.ID)] {
		t.Fatalf("default rerun returns = %v, want only A's %s", got, uuidToString(aReturn.ID))
	}

	// Without an assignment intent, an issue that has accepted returns must
	// name the source explicitly rather than inheriting the worker's latest run.
	dbfx.Exec(t, `DELETE FROM issue_assignment_delegation WHERE issue_id=$1`, issueID)
	refused := serveAuthenticated(t, tokA, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost,
		"/api/issues/"+issueID+"/rerun", map[string]any{}, map[string]string{"id": issueID}, nil)
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "requires task_id") {
		t.Fatalf("unattributed rerun = %d %s, want 400 requiring task_id", refused.Code, refused.Body.String())
	}
}

// UpdateComment uses the same durable obligation path as CreateComment: the
// edited content's accepted target is recorded in the edit transaction, and a
// failed post-insert enqueue is recoverable without another edit.
func TestEditedCommentRecoveredByDurableObligation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, _, token := managedDispatcherFixture(t, "edit-recover")
	target, _ := executableTarget(t, "edit-recover-target")
	issueID := dbfx.Issue(t, "Edited comment durable recovery")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	create := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": "initial body without a request"}, map[string]string{"id": issueID}, nil)
	if create.Code != http.StatusCreated {
		t.Fatalf("create comment = %d: %s", create.Code, create.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created comment: %v", err)
	}

	fn := "work86_edit_fail_" + strings.ReplaceAll(target, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected edited-comment enqueue failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, fn, target, fn, fn))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)

	content := fmt.Sprintf("[@Target](mention://agent/%s) request after edit %d", target, time.Now().UnixNano())
	edit := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch,
		"/api/workspaces/"+testWorkspaceID+"/comments/"+created.ID, map[string]any{"content": content}, map[string]string{"commentId": created.ID}, nil)
	if edit.Code != http.StatusOK {
		t.Fatalf("edit comment = %d: %s", edit.Code, edit.Body.String())
	}
	drop()
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND agent_id=$2 AND status='pending'`, created.ID, target); n != 1 {
		t.Fatalf("pending edited-comment obligation rows = %d, want 1", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, target); n != 0 {
		t.Fatalf("failed edited-comment enqueue left %d runs", n)
	}

	stats, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 10)
	if err != nil {
		t.Fatalf("consume edited-comment obligation: %v", err)
	}
	if stats.Covered != 1 {
		t.Fatalf("consumer covered %d obligations, want 1", stats.Covered)
	}
	runID := latestTaskIDFor(t, issueID, target)
	if runID == "" {
		t.Fatal("consumer created no run for the edited comment")
	}
	var trigger string
	_ = testPool.QueryRow(context.Background(), `SELECT coalesce(trigger_comment_id::text,'') FROM agent_task_queue WHERE id=$1`, runID).Scan(&trigger)
	if trigger != created.ID {
		t.Fatalf("recovered run trigger = %s, want edited comment %s", trigger, created.ID)
	}
	accepted, ok := acceptedReturn(t, issueID)
	if !ok || uuidToString(accepted.DispatcherAgentID) != dispatcher {
		t.Fatalf("accepted return missing: %+v", acceptedReturns(t, issueID))
	}
	requireTaskReturns(t, runID, accepted)
}

// A comment arriving while the same thread is running follows the existing
// queueing behaviour: a fresh follow-up run is created in the comment
// transaction (the unique pending index does not cover 'running'), and that run
// carries only this comment's accepted return. It does not wait for a
// post-commit callback.
func TestActiveRunningCommentQueuesFollowUpWithItsOwnReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	a, _, tokA := managedDispatcherFixture(t, "running-defer-a")
	b, _, tokB := managedDispatcherFixture(t, "running-defer-b")
	x, _ := executableTarget(t, "running-defer-x")
	_ = a
	issueID := dbfx.Issue(t, "Active running follow-up")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A running request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)

	code, c2, body := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) B while running %d", x, time.Now().UnixNano()), c1)
	if code != http.StatusCreated {
		t.Fatalf("active-running comment = %d: %s", code, body)
	}
	if got := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, x); got != 2 {
		t.Fatalf("comment while running produced %d runs, want the active run plus one queued follow-up", got)
	}
	follow := latestTaskIDFor(t, issueID, x)
	if follow == run1 {
		t.Fatal("comment while running created no follow-up run")
	}
	var trigger string
	_ = testPool.QueryRow(context.Background(), `SELECT coalesce(trigger_comment_id::text,'') FROM agent_task_queue WHERE id=$1`, follow).Scan(&trigger)
	if trigger != c2 {
		t.Fatalf("follow-up trigger = %s, want B comment %s", trigger, c2)
	}
	var bReturn string
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == b {
			bReturn = uuidToString(row.ID)
		}
	}
	if got := returnIDs(taskReturnIDs(t, follow)); len(got) != 1 || !got[bReturn] {
		t.Fatalf("follow-up returns = %v, want exactly B's %s", got, bReturn)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='settled'`, c2); n != 1 {
		t.Fatalf("follow-up did not settle the comment obligation")
	}
}
