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

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// cleanupDelegatedIssue removes an API-created issue and every delegation row
// it acquired. TestMain resets the workspace between runs, but within one run
// unrelated list tests assume a small issue population.
func cleanupDelegatedIssue(t *testing.T, issueID string) {
	t.Helper()
	if issueID == "" {
		return
	}
	dbfx.Cleanup(t, `DELETE FROM agent_task_delegation_return WHERE delegation_return_id IN (SELECT id FROM issue_delegation_return WHERE issue_id = $1)`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_delegation_return WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, issueID)
}

// A refused batch delegation must skip the whole item: no assignee, status,
// parent or stage write, no broadcast, and no run. The response reports zero
// updated items, matching the untouched board.
func TestBatchRefusedDelegationLeavesItemUnchanged(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Batch refused runtime")
	dispatcher := dbfx.Agent(t, "Batch refused dispatcher", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Batch refused scope")
	sourceTask := dbfx.Task(t, dispatcher, testutil.Cols{"issue_id": scope, "runtime_id": runtimeID, "status": "running"})
	token := taskTokenFixture(t, dispatcher, sourceTask)
	parentID := dbfx.Issue(t, "Batch refused parent")
	target, _ := executableTarget(t, "batch-refused-target")
	issueID := dbfx.Issue(t, "Batch refused victim")

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.BatchUpdateIssues), http.MethodPost,
		"/api/issues/batch-update", map[string]any{
			"issue_ids": []string{issueID},
			"updates": map[string]any{
				"assignee_type":   "agent",
				"assignee_id":     target,
				"status":          "in_progress",
				"parent_issue_id": parentID,
				"stage":           2,
			},
		}, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("batch status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Updated int `json:"updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	if body.Updated != 0 {
		t.Fatalf("updated = %d, want 0 for a refused item: %s", body.Updated, w.Body.String())
	}
	var assignee, status, parent string
	var stage *int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COALESCE(assignee_id::text,''), status, COALESCE(parent_issue_id::text,''), stage
		FROM issue WHERE id = $1`, issueID).Scan(&assignee, &status, &parent, &stage); err != nil {
		t.Fatalf("load issue: %v", err)
	}
	if assignee != "" || status != "todo" || parent != "" || stage != nil {
		t.Fatalf("refused item changed: assignee=%q status=%q parent=%q stage=%v", assignee, status, parent, stage)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("refused item queued %d runs", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("refused item recorded %d returns", n)
	}
}

// A normal batch status write (no delegation, no run) still updates and is
// counted; the gate must not turn into a blanket skip.
func TestBatchStatusOnlyStillUpdates(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID := dbfx.Issue(t, "Batch status only")
	w := httptest.NewRecorder()
	testHandler.BatchUpdateIssues(w, newRequest(http.MethodPost, "/api/issues/batch-update", map[string]any{
		"issue_ids": []string{issueID},
		"updates":   map[string]any{"status": "in_progress"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("batch status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Updated int `json:"updated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Updated != 1 {
		t.Fatalf("updated = %d, want 1: %s", body.Updated, w.Body.String())
	}
	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("load issue: %v", err)
	}
	if status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", status)
	}
}

// Entering a workflow step with an agent handler by a status-only write starts
// no run, so it must not demand a return address.
func TestWorkflowHandoffStatusOnlyIsNotGated(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Handoff unpinned runtime")
	dispatcher := dbfx.Agent(t, "Handoff unpinned dispatcher", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Handoff unpinned scope")
	sourceTask := dbfx.Task(t, dispatcher, testutil.Cols{"issue_id": scope, "runtime_id": runtimeID, "status": "running"})
	token := taskTokenFixture(t, dispatcher, sourceTask)
	handlerAgent, _ := executableTarget(t, "handoff-handler")

	workflowID := dbfx.Insert(t, "issue_workflow", testutil.Cols{
		"workspace_id":       testWorkspaceID,
		"name":               "Handoff gating workflow",
		"initial_status_key": "todo",
		"steps":              `[{"status_key":"todo"},{"status_key":"in_progress","handler":{"type":"agent","id":"` + handlerAgent + `"}}]`,
	})
	projectID := dbfx.Insert(t, "project", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"title":        "Handoff gating project",
		"workflow_id":  workflowID,
	})
	issueID := dbfx.Issue(t, "Handoff status-only victim")
	dbfx.Exec(t, `UPDATE issue SET project_id = $2, status = 'todo' WHERE id = $1`, issueID, projectID)

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{"status": "in_progress"}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status-only handoff = %d: %s", w.Code, w.Body.String())
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("status-only handoff started %d runs", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("status-only handoff recorded %d returns", n)
	}
}

// A mention that resolves only to a blocked target starts no executable run, so
// the comment keeps its per-target blocked outcome even from an unpinned agent.
func TestBlockedMentionKeepsCommentOutcome(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Blocked mention runtime")
	author := dbfx.Agent(t, "Blocked mention author", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Blocked mention scope")
	authorTask := dbfx.Task(t, author, testutil.Cols{"issue_id": scope, "runtime_id": runtimeID, "status": "running"})
	token := taskTokenFixture(t, author, authorTask)
	issueID := dbfx.Issue(t, "Blocked mention issue")
	const unknownAgent = "11111111-1111-4111-8111-111111111111"
	content := fmt.Sprintf("[@Ghost](mention://agent/%s) nobody home %d", unknownAgent, time.Now().UnixNano())

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("blocked mention = %d: %s", w.Code, w.Body.String())
	}
	var payload struct {
		TriggerOutcomes []struct {
			TargetID   string `json:"target_id"`
			Status     string `json:"status"`
			ReasonCode string `json:"reason_code"`
		} `json:"trigger_outcomes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if len(payload.TriggerOutcomes) != 1 || payload.TriggerOutcomes[0].Status != "blocked" {
		t.Fatalf("outcomes = %+v, want one blocked target", payload.TriggerOutcomes)
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = $2`, issueID, content); n != 1 {
		t.Fatalf("blocked mention saved %d comments", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("blocked mention recorded %d returns", n)
	}
}

// Assigning into backlog starts nothing but must record the dispatcher's
// address; the later start cannot guess it.
func TestBacklogAssignmentRecordsReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "backlog-assign")
	target, _ := executableTarget(t, "backlog-assign")
	issueID := dbfx.Issue(t, "Backlog assign issue")

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
		"/api/issues/"+issueID, map[string]any{
			"assignee_type": "agent",
			"assignee_id":   target,
			"status":        "backlog",
		}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("backlog assignment = %d: %s", w.Code, w.Body.String())
	}
	rows := acceptedReturns(t, issueID)
	if len(rows) != 1 {
		t.Fatalf("backlog assignment returns = %d, want 1: %+v", len(rows), rows)
	}
	if uuidToString(rows[0].DispatcherAgentID) != dispatcher || uuidToString(rows[0].SourceTaskID) != sourceTask {
		t.Fatalf("backlog return = %+v, want %s/%s", rows[0], dispatcher, sourceTask)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("backlog assignment started %d runs", n)
	}
}

// A refused comment delegation must leave neither a comment nor an accepted
// return: the return row commits inside the comment transaction.
func TestRefusedCommentLeavesNoCommentOrReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Comment ghost runtime")
	author := dbfx.Agent(t, "Comment ghost author", runtimeID, testutil.Cols{"execution_kind": "managed"})
	scope := dbfx.Issue(t, "Comment ghost scope")
	authorTask := dbfx.Task(t, author, testutil.Cols{"issue_id": scope, "runtime_id": runtimeID, "status": "running"})
	token := taskTokenFixture(t, author, authorTask)
	target, _ := executableTarget(t, "comment-ghost")
	issueID := dbfx.Issue(t, "Comment ghost issue")
	content := fmt.Sprintf("[@Target](mention://agent/%s) ghost %d", target, time.Now().UnixNano())

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := countValue(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("refused comment wrote %d comments", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("refused comment left %d accepted returns", n)
	}
}

// A squad delegation that starts immediately commits its leader task with the
// issue and its accepted return; a leader-task write failure rolls the whole
// create back.
func TestSquadCreateCommitsLeaderTaskWithReturn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "squad-create")
	leader, _ := executableTarget(t, "squad-leader")
	squadID := dbfx.Insert(t, "squad", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         "Squad create " + fmt.Sprint(time.Now().UnixNano()),
		"description":  "",
		"leader_id":    leader,
		"creator_id":   testUserID,
	})
	title := fmt.Sprintf("squad delegation %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	body := createIssueBody(title, squadID)
	body["assignee_type"] = "squad"

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", body, nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("squad create = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("squad delegation was not created")
	}
	cleanupDelegatedIssue(t, issueID)
	accepted, ok := acceptedReturn(t, issueID)
	if !ok || uuidToString(accepted.DispatcherAgentID) != dispatcher || uuidToString(accepted.SourceTaskID) != sourceTask {
		t.Fatalf("squad accepted return = %+v, want %s/%s", accepted, dispatcher, sourceTask)
	}
	taskID := latestTaskIDFor(t, issueID, leader)
	requireTaskReturns(t, taskID, accepted)

	// Failure injection on the leader task write must roll back the issue and
	// the accepted return together (no post-commit enqueue exemption).
	fn := "work86_fail_squad_leader_" + strings.ReplaceAll(leader, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.agent_id = '%s' THEN
				RAISE EXCEPTION 'injected squad leader task write failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue
		FOR EACH ROW EXECUTE FUNCTION %s();
	`, fn, leader, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	failedTitle := fmt.Sprintf("squad delegation failure %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	failedBody := createIssueBody(failedTitle, squadID)
	failedBody["assignee_type"] = "squad"
	fw := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", failedBody, nil, nil)
	if fw.Code != http.StatusInternalServerError {
		t.Fatalf("injected squad create = %d, want 500: %s", fw.Code, fw.Body.String())
	}
	if _, ok := issueIDByTitle(t, failedTitle); ok {
		t.Fatal("leader-task failure left the squad issue committed")
	}
}

// BindAgentSession must answer 400, not 500, for a non-UUID issue/session id.
func TestBindAgentSessionRejectsNonUUIDInput(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, ownerID := externalAgentFixture(t, "Bind bad input", "delegation-daemon")
	_ = sessionID
	_ = ownerID
	req := withURLParam(newRequest(http.MethodPost, "/api/agents/"+agentID+"/session-bindings", map[string]any{
		"issue_id":   "not-a-uuid",
		"session_id": "also-not-a-uuid",
	}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.BindAgentSession(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// A rerun keeps the source run's accepted set and adds the rerunning agent's
// own accepted relationship.
func TestRerunInheritsSourceRunReturns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	externalAgent, externalSessionID, _ := externalAgentFixture(t, "Rerun inherit external", "delegation-daemon")
	dispatcher, sourceTask, token := managedDispatcherFixture(t, "rerun-inherit")
	target, runtimeID := executableTarget(t, "rerun-inherit")
	issueID := dbfx.Issue(t, "Rerun inherit issue", testutil.Cols{
		"assignee_type": "agent", "assignee_id": target, "status": "in_progress",
	})
	runTask := dbfx.Task(t, target, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "completed",
		"session_id": "rerun-inherit-session", "work_dir": "/work/rerun-inherit",
	})
	inherited, err := delegation.Ensure(context.Background(), testHandler.Queries, testWorkspaceID, issueID, delegation.Assignee{Type: "agent", ID: target}, delegation.Address{
		DispatcherAgentID: externalAgent, Kind: delegation.KindExternalSession, ExternalSessionID: externalSessionID,
	}, testUserID)
	if err != nil {
		t.Fatalf("seed inherited return: %v", err)
	}
	if err := delegation.LinkRun(context.Background(), testHandler.Queries, parseUUID(runTask), inherited.ID); err != nil {
		t.Fatalf("link source return: %v", err)
	}

	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost,
		"/api/issues/"+issueID+"/rerun", map[string]any{"task_id": runTask}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("rerun = %d: %s", w.Code, w.Body.String())
	}
	var own db.IssueDelegationReturn
	for _, row := range acceptedReturns(t, issueID) {
		if uuidToString(row.DispatcherAgentID) == dispatcher {
			own = row
		}
	}
	if !own.ID.Valid {
		t.Fatalf("rerun did not record the rerunning dispatcher's return")
	}
	if uuidToString(own.SourceTaskID) != sourceTask {
		t.Fatalf("rerun own return source = %s, want %s", uuidToString(own.SourceTaskID), sourceTask)
	}
	rerunTaskID := latestTaskIDFor(t, issueID, target)
	requireTaskReturns(t, rerunTaskID, inherited, own)
}

// The CLI must not let a Paseo session with only a human profile silently act
// as a member. This runs the production client construction decision against a
// real Auth middleware: the human PAT is classified as a member and would
// bypass the gate, the marked business command refuses it, and the same client
// with an external agent credential records a real accepted return.
func TestCLIPaseoHumanProfileCannotBypassDelegationGate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	target, _ := executableTarget(t, "cli-paseo-target")
	bumpIssueCounter(t)
	srv := httptest.NewServer(middleware.Auth(testHandler.Queries, nil, nil, nil)(http.HandlerFunc(testHandler.CreateIssue)))
	defer srv.Close()

	humanToken, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	business := &cobra.Command{Use: "issue-create"}
	if err := cli.RequireAgentSessionIdentity(business, humanToken, true, false); err == nil {
		t.Fatal("a Paseo business client accepted the human profile credential")
	}

	// With the human credential the server sees a member and creates the issue
	// without any accepted return: the exact bypass the CLI guard closes.
	humanClient := cli.NewAPIClient(srv.URL, testWorkspaceID, humanToken)
	humanTitle := fmt.Sprintf("cli human bypass %d", time.Now().UnixNano())
	var humanIssue struct {
		ID string `json:"id"`
	}
	if err := humanClient.PostJSON(context.Background(), "/api/issues", createIssueBody(humanTitle, target), &humanIssue); err != nil {
		t.Fatalf("human create: %v", err)
	}
	cleanupDelegatedIssue(t, humanIssue.ID)
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id = $1`, humanIssue.ID); n != 0 {
		t.Fatalf("human actor unexpectedly recorded %d delegation returns", n)
	}

	// The external agent credential for the same client is accepted by real
	// Auth and records the session as the accepted return.
	agentID, sessionID, ownerID := externalAgentFixture(t, "CLI Paseo external", "delegation-daemon")
	var hostSession string
	if err := testPool.QueryRow(context.Background(), `SELECT session_id FROM external_agent_session WHERE id = $1`, sessionID).Scan(&hostSession); err != nil {
		t.Fatalf("load host session: %v", err)
	}
	agentToken := externalTokenFixture(t, agentID, ownerID, time.Now().Add(time.Hour))
	if err := cli.RequireAgentSessionIdentity(business, agentToken, true, false); err != nil {
		t.Fatalf("external agent credential was refused: %v", err)
	}
	agentClient := cli.NewAPIClient(srv.URL, testWorkspaceID, agentToken)
	agentClient.ReturnSession = hostSession
	agentTitle := fmt.Sprintf("cli agent delegation %d", time.Now().UnixNano())
	var agentIssue struct {
		ID string `json:"id"`
	}
	if err := agentClient.PostJSON(context.Background(), "/api/issues", createIssueBody(agentTitle, target), &agentIssue); err != nil {
		t.Fatalf("agent create: %v", err)
	}
	cleanupDelegatedIssue(t, agentIssue.ID)
	rows := acceptedReturns(t, agentIssue.ID)
	if len(rows) != 1 {
		t.Fatalf("agent create returns = %d, want 1: %+v", len(rows), rows)
	}
	if uuidToString(rows[0].DispatcherAgentID) != agentID || uuidToString(rows[0].ExternalSessionID) != sessionID {
		t.Fatalf("agent return = %+v, want dispatcher %s session %s", rows[0], agentID, sessionID)
	}
}
