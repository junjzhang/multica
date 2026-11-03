package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func intentComment(t *testing.T, token, issueID, content, parentID string) (int, string, string) {
	t.Helper()
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", body, map[string]string{"id": issueID}, nil)
	var c struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return w.Code, c.ID, w.Body.String()
}

func extraManagedRun(t *testing.T, agentID, sessionID, workDir string) (string, string) {
	t.Helper()
	var runtimeID string
	if err := testPool.QueryRow(context.Background(), `SELECT runtime_id::text FROM agent WHERE id=$1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load dispatcher runtime: %v", err)
	}
	scope := dbfx.Issue(t, "Managed source scope "+sessionID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":   scope,
		"runtime_id": runtimeID,
		"status":     "running",
		"session_id": sessionID,
		"work_dir":   workDir,
	})
	return taskID, taskTokenFixture(t, agentID, taskID)
}

// The accepted address is fixed per (issue, dispatcher). A continuation turn in
// the same provider session reuses it even though the task id changed; a turn
// from a different physical session is a conflict, never a silent switch.
func TestManagedSameActorIssueContinuationAndSessionConflict(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token1 := managedDispatcherFixture(t, "managed-source")
	targetX, _ := executableTarget(t, "managed-source-x")
	targetY, _ := executableTarget(t, "managed-source-y")
	targetZ, _ := executableTarget(t, "managed-source-z")

	title := fmt.Sprintf("managed source %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token1, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, targetX), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("delegated issue missing")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	first, ok := acceptedReturn(t, issueID)
	if !ok || uuidToString(first.SourceTaskID) != sourceTask {
		t.Fatalf("initial accepted return = %+v, want source %s", first, sourceTask)
	}

	// Same provider session, later turn: a new task id is the same address.
	_, token2 := extraManagedRun(t, dispatcher, "dispatcher-session-managed-source", "/work/dispatcher-managed-source")
	contentY := fmt.Sprintf("[@Y](mention://agent/%s) continuation %d", targetY, time.Now().UnixNano())
	cw := serveAuthenticated(t, token2, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": contentY}, map[string]string{"id": issueID}, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("continuation comment = %d: %s", cw.Code, cw.Body.String())
	}
	reused, ok := acceptedReturn(t, issueID)
	if !ok || reused.ID != first.ID || uuidToString(reused.SourceTaskID) != sourceTask {
		t.Fatalf("continuation replaced the accepted address: %+v vs %+v", reused, first)
	}
	var yComment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &yComment); err != nil {
		t.Fatalf("decode continuation comment: %v", err)
	}
	yTaskID := latestTaskIDFor(t, issueID, targetY)
	requireTaskReturns(t, yTaskID, first)
	if n := countValue(t, `SELECT count(*) FROM comment_delegation_return WHERE comment_id=$1`, yComment.ID); n != 1 {
		t.Fatalf("comment intent rows = %d, want 1", n)
	}

	// A different physical session on the same issue is a conflict.
	_, token3 := extraManagedRun(t, dispatcher, "different-physical-session", "/work/different-physical-session")
	contentZ := fmt.Sprintf("[@Z](mention://agent/%s) other session %d", targetZ, time.Now().UnixNano())
	zw := serveAuthenticated(t, token3, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": contentZ}, map[string]string{"id": issueID}, nil)
	if zw.Code != http.StatusConflict {
		t.Fatalf("different-session comment = %d, want 409: %s", zw.Code, zw.Body.String())
	}
	var refused struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(zw.Body.Bytes(), &refused)
	if refused.Code != "delegation_return_conflict" {
		t.Fatalf("code = %q, want delegation_return_conflict", refused.Code)
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND content=$2`, issueID, contentZ); n != 0 {
		t.Fatalf("conflicting comment saved %d rows", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, targetZ); n != 0 {
		t.Fatalf("conflicting comment created %d runs", n)
	}
	after, ok := acceptedReturn(t, issueID)
	if !ok || after.ID != first.ID || uuidToString(after.SourceTaskID) != sourceTask {
		t.Fatalf("conflict changed the accepted address: %+v", after)
	}
}

// An assignment that coalesces into an already-pending run must attach its
// accepted return to that run, not silently start a parallel dispatch.
func TestAssignmentCoalescesIntoPendingRunWithoutLosingReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, _, token := managedDispatcherFixture(t, "assign-coalesce")
	target, runtimeID := executableTarget(t, "assign-coalesce-target")
	issueID := dbfx.Issue(t, "Assignment coalesce")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	pending := dbfx.Task(t, target, testutil.Cols{
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "queued",
	})

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": target}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("assignment = %d: %s", w.Code, w.Body.String())
	}
	if got := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, target); got != 1 {
		t.Fatalf("coalesced assignment created %d runs, want 1", got)
	}
	var accepted db.IssueDelegationReturn
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == dispatcher {
			accepted = row
		}
	}
	if !accepted.ID.Valid {
		t.Fatalf("accepted return missing: %+v", acceptedReturns(t, issueID))
	}
	requireTaskReturns(t, pending, accepted)
	if n := countValue(t, `SELECT count(*) FROM issue_assignment_delegation WHERE issue_id=$1 AND assignee_id=$2 AND task_id=$3`, issueID, target, pending); n != 1 {
		t.Fatalf("assignment intent was not bound to the coalesced run")
	}
}

// An assignment whose run cannot be inserted must roll the issue write, the
// accepted return, and the assignment intent back together.
func TestAssignmentEnqueueFailureRollsBackWholeAssignment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, _, token := managedDispatcherFixture(t, "assign-rollback")
	target, _ := executableTarget(t, "assign-rollback-target")
	issueID := dbfx.Issue(t, "Assignment enqueue rollback")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	fn := "work86_fail_assign_" + strings.ReplaceAll(target, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected assignment enqueue failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, fn, target, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": target}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("assignment = %d, want the failed write to roll back: %s", w.Code, w.Body.String())
	}
	var assignee struct {
		Type string `json:"assignee_type"`
		ID   string `json:"assignee_id"`
	}
	if err := testPool.QueryRow(context.Background(), `SELECT coalesce(assignee_type,''), coalesce(assignee_id::text,'') FROM issue WHERE id=$1`, issueID).Scan(&assignee.Type, &assignee.ID); err != nil {
		t.Fatalf("load issue assignee: %v", err)
	}
	if assignee.Type != "" || assignee.ID != "" {
		t.Fatalf("failed assignment changed the assignee to %+v", assignee)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID); n != 0 {
		t.Fatalf("failed assignment left %d runs", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1 AND dispatcher_agent_id=$2`, issueID, dispatcher); n != 0 {
		t.Fatalf("failed assignment left %d accepted returns", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_assignment_delegation WHERE issue_id=$1`, issueID); n != 0 {
		t.Fatalf("failed assignment left %d intent rows", n)
	}
}

// B's deferred mention is executed by the follow-up run after X completes.
// That run carries B's accepted return and not the earlier dispatcher's.
func TestDeferredMentionFollowUpCarriesItsOwnReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	a, _, tokA := managedDispatcherFixture(t, "defer-return-a")
	b, _, tokB := managedDispatcherFixture(t, "defer-return-b")
	x, _ := executableTarget(t, "defer-return-x")
	_ = a
	issueID := dbfx.Issue(t, "Deferred follow-up return")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched', dispatched_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)

	code, c2, body := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) B while busy %d", x, time.Now().UnixNano()), c1)
	if code != http.StatusCreated {
		t.Fatalf("deferred comment = %d: %s", code, body)
	}
	if got := latestTaskIDFor(t, issueID, x); got != run1 {
		t.Fatal("deferred mention started a second run while the first was active")
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run1))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)
	follow := latestTaskIDFor(t, issueID, x)
	if follow == run1 {
		t.Fatal("reconcile created no follow-up run")
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
	if bReturn == "" {
		t.Fatalf("B accepted return missing: %+v", acceptedReturns(t, issueID))
	}
	got := returnIDs(taskReturnIDs(t, follow))
	if len(got) != 1 || !got[bReturn] {
		t.Fatalf("follow-up returns = %v, want exactly B's %s", got, bReturn)
	}
}

// While an active run covers the thread, a comment's enqueue is allowed to
// fail without losing the comment: the comment and its accepted relation are
// durable, and the active run's completion replay starts the follow-up that
// carries it.
func TestActiveRunFailureReplaysCommentReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	a, _, tokA := managedDispatcherFixture(t, "active-fail-a")
	b, _, tokB := managedDispatcherFixture(t, "active-fail-b")
	x, _ := executableTarget(t, "active-fail-x")
	_ = a
	issueID := dbfx.Issue(t, "Active run failure replay")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now(), delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)

	fn := "work86_fail_active_" + strings.ReplaceAll(x, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected active-run enqueue failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, fn, x, fn, fn))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)

	code, c2, body := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) B request %d", x, time.Now().UnixNano()), c1)
	if code != http.StatusCreated {
		t.Fatalf("comment = %d, want the comment saved for replay: %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_delegation_return WHERE comment_id=$1`, c2); n != 1 {
		t.Fatalf("comment intent rows = %d, want 1", n)
	}
	drop()

	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run1))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)
	follow := latestTaskIDFor(t, issueID, x)
	if follow == run1 {
		t.Fatal("reconcile created no follow-up")
	}
	var bReturn string
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == b {
			bReturn = uuidToString(row.ID)
		}
	}
	if bReturn == "" {
		t.Fatalf("B accepted return missing: %+v", acceptedReturns(t, issueID))
	}
	if got := returnIDs(taskReturnIDs(t, follow)); len(got) != 1 || !got[bReturn] {
		t.Fatalf("replayed run returns = %v, want exactly B's %s", got, bReturn)
	}
}

// A fold whose return link fails leaves the comment and its accepted relation
// durably saved. Completion replay then starts the run that carries it.
func TestFoldFailureRecoveryCarriesCommentReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	a, _, tokA := managedDispatcherFixture(t, "fold-recovery-a")
	b, _, tokB := managedDispatcherFixture(t, "fold-recovery-b")
	x, _ := executableTarget(t, "fold-recovery-x")
	_ = a
	issueID := dbfx.Issue(t, "Fold failure recovery")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })

	_, c1, _ := intentComment(t, tokA, issueID, fmt.Sprintf("[@X](mention://agent/%s) A request %d", x, time.Now().UnixNano()), "")
	run1 := latestTaskIDFor(t, issueID, x)
	// Mark A's comment delivered so completion replay scopes to B's undelivered
	// comment only.
	dbfx.Exec(t, `UPDATE agent_task_queue SET delivered_comment_ids=ARRAY[$2::uuid] WHERE id=$1`, run1, c1)

	fn := "work86_fail_fold_" + strings.ReplaceAll(b, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM issue_delegation_return r
			           WHERE r.id = NEW.delegation_return_id AND r.dispatcher_agent_id = '%s') THEN
				RAISE EXCEPTION 'injected fold link failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_delegation_return
		FOR EACH ROW EXECUTE FUNCTION %s();`, fn, b, fn, fn))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)

	code, c2, body := intentComment(t, tokB, issueID, fmt.Sprintf("[@X](mention://agent/%s) B request %d", x, time.Now().UnixNano()), c1)
	if code != http.StatusCreated {
		t.Fatalf("fold comment = %d: %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE id=$1`, c2); n != 1 {
		t.Fatalf("fold failure lost the comment (%d rows)", n)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_delegation_return WHERE comment_id=$1`, c2); n != 1 {
		t.Fatalf("fold failure lost the comment's accepted relation (%d rows)", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND $2::uuid = ANY(coalesced_comment_ids)`, run1, c2); n != 0 {
		t.Fatal("failed fold still attached the comment to the run")
	}
	drop()

	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, run1)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(run1))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.reconcileCommentsOnCompletion(context.Background(), &task)
	follow := latestTaskIDFor(t, issueID, x)
	if follow == run1 {
		t.Fatal("reconcile created no follow-up run")
	}
	var bReturn string
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == b {
			bReturn = uuidToString(row.ID)
		}
	}
	if bReturn == "" {
		t.Fatalf("B accepted return missing: %+v", acceptedReturns(t, issueID))
	}
	if got := returnIDs(taskReturnIDs(t, follow)); len(got) != 1 || !got[bReturn] {
		t.Fatalf("recovered run returns = %v, want exactly B's %s", got, bReturn)
	}
}
