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

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// waitForBackendBlockedBy observes a real backend waiting on a lock held by
// holderPID. It replaces a fixed sleep in the concurrency counterexamples.
func waitForBackendBlockedBy(t *testing.T, holderPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := testPool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE $1::int = ANY(pg_blocking_pids(pid))
			)`, holderPID).Scan(&waiting); err != nil {
			t.Fatalf("observe lock waiter: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delegation write never reached the held row lock")
}

func lockHolderPID(t *testing.T, tx pgx.Tx) int32 {
	t.Helper()
	var pid int32
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read lock holder pid: %v", err)
	}
	return pid
}

func returnsForAssignee(t *testing.T, issueID string, assigneeType, assigneeID string) []db.IssueDelegationReturn {
	t.Helper()
	var out []db.IssueDelegationReturn
	for _, row := range acceptedReturns(t, issueID) {
		if row.AssigneeType == assigneeType && uuidToString(row.AssigneeID) == assigneeID {
			out = append(out, row)
		}
	}
	return out
}

// A rerun refused by triage, the invoke gate, or the enqueue itself must leave
// no accepted return behind. The address is recorded only in the same
// transaction that creates the run.
func TestRerunRefusalLeavesNoAcceptedReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	t.Run("triage", func(t *testing.T) {
		dispatcher, sourceTask, token := managedDispatcherFixture(t, "rerun-refuse-triage")
		target, _ := executableTarget(t, "rerun-refuse-triage")
		issueID := dbfx.Issue(t, "Rerun triage refusal", testutil.Cols{
			"assignee_type": "agent", "assignee_id": target, "status": "todo", "triage_state": "pending",
		})

		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost,
			"/api/issues/"+issueID+"/rerun", map[string]any{}, map[string]string{"id": issueID}, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("triage rerun = %d, want 403: %s", w.Code, w.Body.String())
		}
		if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE dispatcher_agent_id = $1 AND source_task_id = $2`, dispatcher, sourceTask); n != 0 {
			t.Fatalf("refused rerun left %d accepted returns", n)
		}
		if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, target); n != 0 {
			t.Fatalf("refused rerun created %d tasks", n)
		}
	})

	t.Run("invoke", func(t *testing.T) {
		dispatcher, sourceTask, token := managedDispatcherFixture(t, "rerun-refuse-invoke")
		runtimeID := dbfx.Runtime(t, "Rerun private target runtime")
		owner := dbfx.User(t, "Rerun private target owner", fmt.Sprintf("rerun-private-%d@test", time.Now().UnixNano()))
		dbfx.Member(t, testWorkspaceID, owner, "member")
		target := dbfx.Agent(t, "Rerun private target", runtimeID, testutil.Cols{
			"execution_kind": "managed", "visibility": "private", "owner_id": owner,
		})
		issueID := dbfx.Issue(t, "Rerun invoke refusal", testutil.Cols{
			"assignee_type": "agent", "assignee_id": target, "status": "todo",
		})

		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost,
			"/api/issues/"+issueID+"/rerun", map[string]any{}, map[string]string{"id": issueID}, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("denied rerun = %d, want 403: %s", w.Code, w.Body.String())
		}
		if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE dispatcher_agent_id = $1 AND source_task_id = $2`, dispatcher, sourceTask); n != 0 {
			t.Fatalf("denied rerun left %d accepted returns", n)
		}
	})

	t.Run("enqueue", func(t *testing.T) {
		dispatcher, sourceTask, token := managedDispatcherFixture(t, "rerun-refuse-enqueue")
		target, _ := executableTarget(t, "rerun-refuse-enqueue")
		issueID := dbfx.Issue(t, "Rerun enqueue failure", testutil.Cols{
			"assignee_type": "agent", "assignee_id": target, "status": "todo",
		})
		fn := "work86_fail_rerun_" + strings.ReplaceAll(target, "-", "_")
		dbfx.Exec(t, fmt.Sprintf(`
			CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
			BEGIN
				IF NEW.agent_id = '%s' THEN
					RAISE EXCEPTION 'injected rerun task write failure';
				END IF;
				RETURN NEW;
			END $$ LANGUAGE plpgsql;
			CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue
			FOR EACH ROW EXECUTE FUNCTION %s();
		`, fn, target, fn, fn))
		t.Cleanup(func() {
			dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
		})

		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost,
			"/api/issues/"+issueID+"/rerun", map[string]any{}, map[string]string{"id": issueID}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("enqueue-failed rerun = %d, want 400: %s", w.Code, w.Body.String())
		}
		if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE dispatcher_agent_id = $1 AND source_task_id = $2`, dispatcher, sourceTask); n != 0 {
			t.Fatalf("failed rerun enqueue left %d accepted returns", n)
		}
		if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, target); n != 0 {
			t.Fatalf("failed rerun enqueue left %d tasks", n)
		}
	})
}

// The fold into a pending run and the accepted relationship it adds are one
// transaction. When the link fails the fold rolls back too, so no run carries
// the comment while silently missing its dispatcher. The accepted return itself
// stays durable and a later fold attaches it.
func TestCoalescedCommentLinkRollsBackWithFold(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	firstAgent, firstTask, firstToken := managedDispatcherFixture(t, "coalesce-atomic-first")
	secondAgent, secondTask, secondToken := managedDispatcherFixture(t, "coalesce-atomic-second")
	target, _ := executableTarget(t, "coalesce-atomic-target")
	issueID := dbfx.Issue(t, "Coalesced atomic link")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	post := func(token, content, parentID string) *httptest.ResponseRecorder {
		body := map[string]any{"content": content}
		if parentID != "" {
			body["parent_id"] = parentID
		}
		return serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
			"/api/issues/"+issueID+"/comments", body, map[string]string{"id": issueID}, nil)
	}

	firstBody := fmt.Sprintf("[@Target](mention://agent/%s) first %d", target, time.Now().UnixNano())
	first := post(firstToken, firstBody, "")
	if first.Code != http.StatusCreated {
		t.Fatalf("first mention = %d: %s", first.Code, first.Body.String())
	}
	var firstComment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstComment); err != nil {
		t.Fatalf("decode first comment: %v", err)
	}
	targetTaskID := latestTaskIDFor(t, issueID, target)
	firstRows := returnsForAssignee(t, issueID, "agent", target)
	if len(firstRows) != 1 || uuidToString(firstRows[0].DispatcherAgentID) != firstAgent || uuidToString(firstRows[0].SourceTaskID) != firstTask {
		t.Fatalf("first accepted return = %+v, want %s/%s", firstRows, firstAgent, firstTask)
	}

	fn := "work86_fail_comment_link_" + strings.ReplaceAll(secondAgent, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM issue_delegation_return r
			           WHERE r.id = NEW.delegation_return_id AND r.dispatcher_agent_id = '%s') THEN
				RAISE EXCEPTION 'injected coalesced link failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_delegation_return
		FOR EACH ROW EXECUTE FUNCTION %s();
	`, fn, secondAgent, fn, fn))
	dropInjected := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(dropInjected)

	secondBody := fmt.Sprintf("[@Target](mention://agent/%s) second %d", target, time.Now().UnixNano())
	second := post(secondToken, secondBody, firstComment.ID)
	if second.Code != http.StatusCreated {
		t.Fatalf("second mention = %d, want the comment to stay saved: %s", second.Code, second.Body.String())
	}
	var secondComment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondComment); err != nil {
		t.Fatalf("decode second comment: %v", err)
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = $2`, issueID, secondBody); n != 1 {
		t.Fatalf("second comment persisted %d rows, want 1", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND trigger_comment_id = $2`, targetTaskID, firstComment.ID); n != 1 {
		t.Fatal("failed link changed the run's trigger comment")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND $2::uuid = ANY(coalesced_comment_ids)`, targetTaskID, secondComment.ID); n != 0 {
		t.Fatal("failed link left the comment folded into the run")
	}
	secondReturn := returnsForAssignee(t, issueID, "agent", target)
	var secondAccepted db.IssueDelegationReturn
	for _, row := range secondReturn {
		if uuidToString(row.DispatcherAgentID) == secondAgent {
			secondAccepted = row
		}
	}
	if !secondAccepted.ID.Valid || uuidToString(secondAccepted.SourceTaskID) != secondTask {
		t.Fatalf("accepted return for the second dispatcher is missing: %+v", secondReturn)
	}
	if got := returnIDs(taskReturnIDs(t, targetTaskID)); got[uuidToString(secondAccepted.ID)] {
		t.Fatal("failed link attached the second dispatcher to the run")
	}

	// Removing the fault lets the next request fold and link both atomically.
	dropInjected()
	thirdBody := fmt.Sprintf("[@Target](mention://agent/%s) third %d", target, time.Now().UnixNano())
	third := post(secondToken, thirdBody, firstComment.ID)
	if third.Code != http.StatusCreated {
		t.Fatalf("third mention = %d: %s", third.Code, third.Body.String())
	}
	var thirdComment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(third.Body.Bytes(), &thirdComment); err != nil {
		t.Fatalf("decode third comment: %v", err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND trigger_comment_id = $2`, targetTaskID, thirdComment.ID); n != 1 {
		t.Fatal("successful fold did not record the new trigger comment")
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND $2::uuid = ANY(coalesced_comment_ids)`, targetTaskID, firstComment.ID); n != 1 {
		t.Fatal("successful fold did not preserve the previous trigger comment")
	}
	requireTaskReturns(t, targetTaskID, firstRows[0], secondAccepted)
}

// A backlog delegation stays attached to its explicit assignment slot even
// after the same dispatcher starts another worker's run. Starting the backlog
// assignee later attaches the original relationship and nothing else.
func TestBacklogReturnSurvivesAnotherDispatchAndStartsWithItsOwnRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "backlog-survives")
	targetB, _ := executableTarget(t, "backlog-survives-b")
	targetC, _ := executableTarget(t, "backlog-survives-c")
	title := fmt.Sprintf("backlog survives %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	body := createIssueBody(title, targetB)
	body["status"] = "backlog"
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", body, nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("backlog create = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("backlog issue was not created")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	backlogRows := returnsForAssignee(t, issueID, "agent", targetB)
	if len(backlogRows) != 1 || uuidToString(backlogRows[0].DispatcherAgentID) != dispatcher || uuidToString(backlogRows[0].SourceTaskID) != sourceTask {
		t.Fatalf("backlog accepted return = %+v", backlogRows)
	}
	backlogReturnID := uuidToString(backlogRows[0].ID)
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("backlog create started %d runs", n)
	}

	content := fmt.Sprintf("[@C](mention://agent/%s) backlog other dispatch %d", targetC, time.Now().UnixNano())
	cw := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("other dispatch = %d: %s", cw.Code, cw.Body.String())
	}
	cTaskID := latestTaskIDFor(t, issueID, targetC)
	cReturns := taskReturnIDs(t, cTaskID)
	if len(cReturns) != 1 {
		t.Fatalf("other worker's run returns = %d, want only its own dispatch: %+v", len(cReturns), cReturns)
	}
	if got := returnIDs(cReturns); got[backlogReturnID] {
		t.Fatal("other worker's run carried the backlog relationship")
	}

	// Unassign, then explicitly assign B while leaving backlog: the start must
	// attach A's original backlog relationship.
	uw := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": nil, "assignee_id": nil}, map[string]string{"id": issueID}, nil)
	if uw.Code != http.StatusOK {
		t.Fatalf("unassign = %d: %s", uw.Code, uw.Body.String())
	}
	aw := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": targetB, "status": "todo"}, map[string]string{"id": issueID}, nil)
	if aw.Code != http.StatusOK {
		t.Fatalf("start backlog assignee = %d: %s", aw.Code, aw.Body.String())
	}
	bTaskID := latestTaskIDFor(t, issueID, targetB)
	bReturns := taskReturnIDs(t, bTaskID)
	if got := returnIDs(bReturns); !got[backlogReturnID] {
		t.Fatalf("started backlog run lost the original return: got %+v want %s", bReturns, backlogReturnID)
	}
	if len(bReturns) != 1 {
		t.Fatalf("started backlog run returns = %d, want only the backlog relationship: %+v", len(bReturns), bReturns)
	}
}

// Reassigning X to Z preserves the replaced assignment's recipients and adds
// the reassigner's own row. A different worker's independent dispatch on the
// same issue (B -> Y) must not leak into Z's run.
func TestReassignmentKeepsOnlyReplacedAssignmentsReturns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcherA, sourceA, tokenA := managedDispatcherFixture(t, "reassign-scope-a")
	dispatcherB, sourceB, tokenB := managedDispatcherFixture(t, "reassign-scope-b")
	targetX, _ := executableTarget(t, "reassign-scope-x")
	targetY, _ := executableTarget(t, "reassign-scope-y")
	targetZ, _ := executableTarget(t, "reassign-scope-z")

	title := fmt.Sprintf("reassign scope %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, tokenA, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, targetX), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create assigned X = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("issue was not created")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	xTaskID := latestTaskIDFor(t, issueID, targetX)

	content := fmt.Sprintf("[@Y](mention://agent/%s) independent %d", targetY, time.Now().UnixNano())
	cw := serveAuthenticated(t, tokenB, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("independent Y dispatch = %d: %s", cw.Code, cw.Body.String())
	}
	yTaskID := latestTaskIDFor(t, issueID, targetY)

	rw := serveAuthenticated(t, tokenA, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"assignee_type": "agent", "assignee_id": targetZ}, map[string]string{"id": issueID}, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("reassign X -> Z = %d: %s", rw.Code, rw.Body.String())
	}
	zTaskID := latestTaskIDFor(t, issueID, targetZ)

	ax := returnsForAssignee(t, issueID, "agent", targetX)
	by := returnsForAssignee(t, issueID, "agent", targetY)
	az := returnsForAssignee(t, issueID, "agent", targetZ)
	if len(ax) != 1 || uuidToString(ax[0].DispatcherAgentID) != dispatcherA || uuidToString(ax[0].SourceTaskID) != sourceA {
		t.Fatalf("X return = %+v", ax)
	}
	if len(by) != 1 || uuidToString(by[0].DispatcherAgentID) != dispatcherB || uuidToString(by[0].SourceTaskID) != sourceB {
		t.Fatalf("Y return = %+v", by)
	}
	if len(az) != 1 || uuidToString(az[0].DispatcherAgentID) != dispatcherA || uuidToString(az[0].SourceTaskID) != sourceA {
		t.Fatalf("Z return = %+v", az)
	}
	if got := returnIDs(taskReturnIDs(t, xTaskID)); len(got) != 1 || !got[uuidToString(ax[0].ID)] {
		t.Fatalf("X run returns = %v, want only %s", got, uuidToString(ax[0].ID))
	}
	if got := returnIDs(taskReturnIDs(t, yTaskID)); len(got) != 1 || !got[uuidToString(by[0].ID)] {
		t.Fatalf("Y run returns = %v, want only %s", got, uuidToString(by[0].ID))
	}
	zReturns := returnIDs(taskReturnIDs(t, zTaskID))
	if len(zReturns) != 2 || !zReturns[uuidToString(ax[0].ID)] || !zReturns[uuidToString(az[0].ID)] {
		t.Fatalf("Z run returns = %v, want exactly the replaced X row and the reassigner's own row", zReturns)
	}
	if zReturns[uuidToString(by[0].ID)] {
		t.Fatal("reassignment leaked the independent Y dispatch into Z's run")
	}
}

// A managed request that passed authentication but blocked on the dispatching
// run's row must re-check that run after the blocker commits. Completed,
// unpinned, and archived dispatchers are refused before any child work.
func TestDelegationSourceRecheckedInsideWriteTransaction(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name       string
		lockTable  string
		action     string
		wantStatus int
		wantCode   string
	}{
		{
			name: "completed", lockTable: "agent_task_queue",
			action:     `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`,
			wantStatus: http.StatusConflict, wantCode: "delegation_return_source_closed",
		},
		{
			name: "unpinned", lockTable: "agent_task_queue",
			action:     `UPDATE agent_task_queue SET session_id = NULL WHERE id = $1`,
			wantStatus: http.StatusConflict, wantCode: "delegation_return_not_ready",
		},
		{
			name: "archived", lockTable: "agent",
			action:     `UPDATE agent SET archived_at = now() WHERE id = $1`,
			wantStatus: http.StatusBadRequest, wantCode: "delegation_return_invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher, sourceTask, token := managedDispatcherFixture(t, "source-race-"+tc.name)
			target, _ := executableTarget(t, "source-race-"+tc.name)
			title := fmt.Sprintf("source race %s %d", tc.name, time.Now().UnixNano())
			bumpIssueCounter(t)
			lockID := sourceTask
			if tc.lockTable == "agent" {
				lockID = dispatcher
			}

			holder, err := testPool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			defer holder.Rollback(context.Background())
			if _, err := holder.Exec(context.Background(), `SELECT id FROM `+tc.lockTable+` WHERE id = $1 FOR UPDATE`, lockID); err != nil {
				t.Fatalf("hold %s row: %v", tc.lockTable, err)
			}
			holderPID := lockHolderPID(t, holder)

			type response struct {
				rec *httptest.ResponseRecorder
			}
			done := make(chan response, 1)
			go func() {
				rec := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost,
					"/api/issues", createIssueBody(title, target), nil, nil)
				done <- response{rec: rec}
			}()
			waitForBackendBlockedBy(t, holderPID)
			if _, err := holder.Exec(context.Background(), tc.action, lockID); err != nil {
				t.Fatalf("apply %s action: %v", tc.name, err)
			}
			if err := holder.Commit(context.Background()); err != nil {
				t.Fatalf("commit holder: %v", err)
			}
			rec := (<-done).rec
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s create = %d, want %d: %s", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
			}
			var payload struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &payload)
			if payload.Code != tc.wantCode {
				t.Fatalf("%s code = %q, want %q: %s", tc.name, payload.Code, tc.wantCode, rec.Body.String())
			}
			if _, ok := issueIDByTitle(t, title); ok {
				t.Fatalf("%s refusal left the delegated issue committed", tc.name)
			}
			if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE dispatcher_agent_id = $1 AND source_task_id = $2`, dispatcher, sourceTask); n != 0 {
				t.Fatalf("%s refusal left %d accepted returns", tc.name, n)
			}
		})
	}
}

// A completed source stays a valid target for the relationships it already
// accepted, but it cannot accept a new delegation. The two facts are separate.
func TestCompletedSourceRemainsStoredReturnTarget(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "completed-target")
	target, _ := executableTarget(t, "completed-target")
	title := fmt.Sprintf("completed target %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("delegated create = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("delegated issue was not created")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	rows := returnsForAssignee(t, issueID, "agent", target)
	if len(rows) != 1 {
		t.Fatalf("accepted returns = %d, want 1", len(rows))
	}
	childTask := latestTaskIDFor(t, issueID, target)
	requireTaskReturns(t, childTask, rows[0])

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, sourceTask)
	requireTaskReturns(t, childTask, rows[0])

	againTitle := fmt.Sprintf("completed source new delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	aw := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost,
		"/api/issues", createIssueBody(againTitle, target), nil, nil)
	if aw.Code != http.StatusConflict {
		t.Fatalf("new delegation from completed source = %d, want 409: %s", aw.Code, aw.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(aw.Body.Bytes(), &payload)
	if payload.Code != "delegation_return_source_closed" {
		t.Fatalf("code = %q, want delegation_return_source_closed", payload.Code)
	}
	if _, ok := issueIDByTitle(t, againTitle); ok {
		t.Fatal("completed source created a new delegated issue")
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE dispatcher_agent_id = $1 AND source_task_id = $2`, dispatcher, sourceTask); n != 1 {
		t.Fatalf("completed source accepted %d relationships, want the original one only", n)
	}
}

// One comment that mentions two workers is two dispatches. Each run carries the
// accepted relationship for its own assignment slot; the other worker's row is
// never attached to it.
func TestCommentToTwoTargetsKeepsEachReturnOnItsOwnRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "multi-target")
	targetA, _ := executableTarget(t, "multi-target-a")
	targetB, _ := executableTarget(t, "multi-target-b")
	issueID := dbfx.Issue(t, "Multi-target comment")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	content := fmt.Sprintf("[@A](mention://agent/%s) [@B](mention://agent/%s) both %d", targetA, targetB, time.Now().UnixNano())

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("multi-target comment = %d: %s", w.Code, w.Body.String())
	}
	rows := acceptedReturns(t, issueID)
	if len(rows) != 2 {
		t.Fatalf("accepted returns = %d, want one per target: %+v", len(rows), rows)
	}
	byAssignee := map[string]db.IssueDelegationReturn{}
	for _, row := range rows {
		if uuidToString(row.DispatcherAgentID) != dispatcher || uuidToString(row.SourceTaskID) != sourceTask {
			t.Fatalf("accepted return = %+v, want dispatcher %s source %s", row, dispatcher, sourceTask)
		}
		byAssignee[uuidToString(row.AssigneeID)] = row
	}
	for _, target := range []string{targetA, targetB} {
		taskID := latestTaskIDFor(t, issueID, target)
		got := taskReturnIDs(t, taskID)
		own, ok := byAssignee[target]
		if !ok {
			t.Fatalf("no accepted return for target %s", target)
		}
		if len(got) != 1 || uuidToString(got[0].ID) != uuidToString(own.ID) {
			t.Fatalf("target %s run returns = %+v, want only %s", target, got, uuidToString(own.ID))
		}
	}
}

// A comment that starts a new run persists the run and its accepted return in
// one transaction. A link failure rolls the run back, so executable work never
// exists without the dispatcher it must return to.
func TestNewCommentRunLinkFailureRollsBackRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, _, token := managedDispatcherFixture(t, "new-run-link")
	target, _ := executableTarget(t, "new-run-link")
	issueID := dbfx.Issue(t, "New run link atomic")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	fn := "work86_fail_new_run_link_" + strings.ReplaceAll(dispatcher, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM issue_delegation_return r
			           WHERE r.id = NEW.delegation_return_id AND r.dispatcher_agent_id = '%s') THEN
				RAISE EXCEPTION 'injected new-run link failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_delegation_return
		FOR EACH ROW EXECUTE FUNCTION %s();
	`, fn, dispatcher, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})

	content := fmt.Sprintf("[@Target](mention://agent/%s) new run link %d", target, time.Now().UnixNano())
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("comment = %d, want the comment saved: %s", w.Code, w.Body.String())
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = $2`, issueID, content); n != 1 {
		t.Fatalf("comment rows = %d, want 1", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, target); n != 0 {
		t.Fatalf("failed link left %d executable runs", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id = $1 AND dispatcher_agent_id = $2`, issueID, dispatcher); n != 1 {
		t.Fatalf("accepted return rows = %d, want the durable comment return", n)
	}
}
