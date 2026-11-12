package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func sameOwnerExternalCaller(t *testing.T, name string) (string, string, string) {
	t.Helper()
	agent := dbfx.Agent(t, name, "", testutil.Cols{"execution_kind": "external", "owner_id": testUserID, "permission_mode": "private"})
	session := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "owner_user_id": testUserID, "agent_id": agent,
		"provider": "paseo", "host_id": "review-host", "session_id": name,
		"daemon_id": "review-probe-daemon",
	})
	return agent, session, externalTokenFixture(t, agent, testUserID, time.Now().Add(time.Hour))
}

func TestIndependentDelegationsSharingSessionKeepOwnUpstream(t *testing.T) {
	ctx := context.Background()
	root1, rootSession1, token1 := sameOwnerExternalCaller(t, "review-root-one")
	root2, rootSession2, token2 := sameOwnerExternalCaller(t, "review-root-two")
	worker, runtimeID := executableTarget(t, "review-independent-worker")
	reviewer, _ := executableTarget(t, "review-independent-reviewer")
	dbfx.Exec(t, `UPDATE agent_runtime SET runtime_mode='local',provider='codex',daemon_id='review-independent-daemon' WHERE id=$1`, runtimeID)
	title := fmt.Sprintf("independent external callers %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token1, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, worker), nil, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, rootSession1) })
	if w.Code != http.StatusCreated {
		t.Fatalf("first external dispatch failed: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	start := func(id string) db.AgentTaskQueue {
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),session_id='review-shared-session',work_dir='/work/review-shared',resource_control_required=true,session_pin_confirmed=true,session_pin_generation=gen_random_uuid() WHERE id=$1`, id)
		row, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	first := start(latestTaskIDFor(t, issueID, worker))
	firstParents := taskReturnIDs(t, uuidToString(first.ID))
	if len(firstParents) != 1 || uuidToString(firstParents[0].DispatcherAgentID) != root1 {
		t.Fatalf("wrong first upstream: %+v", firstParents)
	}
	requestReview := func(source db.AgentTaskQueue, message string) db.AgentTaskQueue {
		token := taskTokenFixture(t, worker, uuidToString(source.ID))
		code, _, body := intentComment(t, token, issueID, fmt.Sprintf("[@Review](mention://agent/%s) %s", reviewer, message), uuidToString(source.TriggerCommentID))
		if code != http.StatusCreated {
			t.Fatalf("review dispatch failed: %d %s", code, body)
		}
		row, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, issueID, reviewer)))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	complete := func(source db.AgentTaskQueue) {
		_, err := testHandler.TaskService.CompleteTask(service.WithTerminalReport(ctx), source.ID, []byte(`{"output":"Awaiting review"}`), source.SessionID.String, source.WorkDir.String, "", false, "", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	firstReview := requestReview(first, "review first external request")
	complete(first)
	finishResultFixture(t, firstReview)
	firstReturn, firstClaim := startResultHandlingTurn(t, firstReview)
	completeContinuation(t, firstReturn, firstClaim, "First external request reviewed")
	w = serveAuthenticated(t, token2, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": fmt.Sprintf("[@Worker](mention://agent/%s) implement independent second request", worker)}, map[string]string{"id": issueID}, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, rootSession2) })
	if w.Code != http.StatusCreated {
		t.Fatalf("second external dispatch failed: %d %s", w.Code, w.Body.String())
	}
	secondID := latestTaskIDFor(t, issueID, worker)
	if secondID == uuidToString(first.ID) || secondID == uuidToString(firstReturn.ID) {
		t.Fatalf("no new independent caller run: %s", w.Body.String())
	}
	second := start(secondID)
	secondParents := taskReturnIDs(t, secondID)
	if len(secondParents) != 1 || uuidToString(secondParents[0].DispatcherAgentID) != root2 {
		t.Fatalf("second request wasn't isolated: %+v", secondParents)
	}
	secondReview := requestReview(second, "review independent second external request")
	if secondReview.ID == firstReview.ID {
		t.Fatal("second review reused completed first run")
	}
	returnedTo := taskReturnIDs(t, uuidToString(secondReview.ID))
	if len(returnedTo) != 1 || returnedTo[0].SourceTaskID != first.ID {
		t.Fatalf("did not exercise same physical address reuse: %+v", returnedTo)
	}
	complete(second)
	finishResultFixture(t, secondReview)
	continued, _ := startResultHandlingTurn(t, secondReview)
	t.Logf("HTTP root1=%s root2=%s firstTask=%s secondTask=%s finalReturns=%+v", root1, root2, uuidToString(first.ID), secondID, taskReturnIDs(t, uuidToString(continued.ID)))
	requireUpstreamReturns(t, continued, secondParents[0])
}

func nextCaller(t *testing.T, source db.AgentTaskQueue, overrides testutil.Cols) db.AgentTaskQueue {
	t.Helper()
	cols := testutil.Cols{
		"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running",
		"session_id": source.SessionID.String, "work_dir": source.WorkDir.String,
		"resource_control_required": true, "session_pin_confirmed": true,
		"session_pin_generation": testutil.Raw("gen_random_uuid()"),
		"originator_user_id":     source.OriginatorUserID, "accountable_user_id": source.AccountableUserID,
	}
	for key, value := range overrides {
		cols[key] = value
	}
	id := dbfx.Task(t, uuidToString(source.AgentID), cols)
	row, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func callerReview(t *testing.T, caller, review db.AgentTaskQueue, parent, message string) (db.AgentTaskQueue, string) {
	t.Helper()
	token := taskTokenFixture(t, uuidToString(caller.AgentID), uuidToString(caller.ID))
	code, comment, body := intentComment(t, token, uuidToString(review.IssueID), fmt.Sprintf("[@Review](mention://agent/%s) %s", uuidToString(review.AgentID), message), parent)
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %s", code, body)
	}
	row, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(latestTaskIDFor(t, uuidToString(review.IssueID), uuidToString(review.AgentID))))
	if err != nil {
		t.Fatal(err)
	}
	return row, comment
}

func TestCoalescedCallersPreserveBothUpstreamSets(t *testing.T) {
	first, initial, address := resultFixture(t, "coalesced-callers")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, initial.ID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, first.ID)
	one := externalUpstream(t, first, "coalesced-one", true)
	pending, comment := callerReview(t, first, initial, "", "first request")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, first.ID)
	second := nextCaller(t, first, nil)
	two := externalUpstream(t, second, "coalesced-two", true)
	folded, _ := callerReview(t, second, initial, comment, "second request")
	if pending.ID != folded.ID {
		t.Fatal("second request did not coalesce")
	}
	var callers []string
	dbfx.QueryRow(t, `SELECT caller_task_ids::text[] FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2`, folded.ID, address.ID).Scan(&callers)
	if len(callers) != 2 {
		t.Fatalf("coalesced run lost caller snapshot: %v", callers)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=ANY($1::uuid[])`, []string{uuidToString(first.ID), uuidToString(second.ID)})
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now(),max_attempts=3 WHERE id=$1`, folded.ID)
	if _, err := testHandler.TaskService.FailTask(service.WithTerminalReport(context.Background()), folded.ID, "network", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatal(err)
	}
	retry, err := testHandler.Queries.GetAutomaticRetryChild(context.Background(), folded.ID)
	if err != nil {
		t.Fatal(err)
	}
	finishResultFixture(t, retry)
	continued, _ := startResultHandlingTurn(t, retry)
	if continued.DelegatedFromTaskID.Valid {
		t.Fatal("multiple callers collapsed to a singular audit lineage")
	}
	requireUpstreamReturns(t, continued, one, two)
}

func TestCommentEditReplacesCallerWithoutRewritingPastRun(t *testing.T) {
	first, initial, address := resultFixture(t, "edited-callers")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, initial.ID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, first.ID)
	one := externalUpstream(t, first, "edited-one", true)
	oldRun, comment := callerReview(t, first, initial, "", "original request")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, first.ID)
	finishResultFixture(t, oldRun)
	oldTurn, oldClaim := startResultHandlingTurn(t, oldRun)
	completeContinuation(t, oldTurn, oldClaim, "Original request handled")
	second := nextCaller(t, first, nil)
	two := externalUpstream(t, second, "edited-two", true)
	token := taskTokenFixture(t, uuidToString(second.AgentID), uuidToString(second.ID))
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch, "/api/comments/"+comment,
		map[string]any{"content": fmt.Sprintf("[@Review](mention://agent/%s) changed request", uuidToString(initial.AgentID))}, map[string]string{"commentId": comment}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	var oldCallers, newCallers []string
	dbfx.QueryRow(t, `SELECT caller_task_ids::text[] FROM agent_task_delegation_return WHERE task_id=$1 AND delegation_return_id=$2`, oldRun.ID, address.ID).Scan(&oldCallers)
	dbfx.QueryRow(t, `SELECT caller_task_ids::text[] FROM comment_delegation_return WHERE comment_id=$1 AND delegation_return_id=$2`, comment, address.ID).Scan(&newCallers)
	if len(oldCallers) != 1 || oldCallers[0] != uuidToString(first.ID) || len(newCallers) != 1 || newCallers[0] != uuidToString(second.ID) {
		t.Fatalf("edit rewrote historical provenance or retained old intent: old=%v new=%v", oldCallers, newCallers)
	}
	newRun, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(latestTaskIDFor(t, uuidToString(initial.IssueID), uuidToString(initial.AgentID))))
	if err != nil {
		t.Fatal(err)
	}
	if newRun.ID == oldRun.ID {
		t.Fatal("edit did not create a new generation")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, second.ID)
	finishResultFixture(t, newRun)
	newTurn, _ := startResultHandlingTurn(t, newRun)
	requireUpstreamReturns(t, oldTurn, one)
	requireUpstreamReturns(t, newTurn, two)
}

func TestResultAuthorityFollowsActualCaller(t *testing.T) {
	first, initial, _ := resultFixture(t, "caller-authority")
	finishResultFixture(t, initial)
	oldTurn, claim := startResultHandlingTurn(t, initial)
	completeContinuation(t, oldTurn, claim, "First request handled")
	other := dbfx.User(t, "Different caller", "caller-authority@example.com")
	dbfx.Member(t, testWorkspaceID, other, "member")
	second := nextCaller(t, first, testutil.Cols{"originator_user_id": other, "accountable_user_id": other})
	review, _ := callerReview(t, second, initial, "", "request from another human")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, second.ID)
	finishResultFixture(t, review)
	continued, _ := startResultHandlingTurn(t, review)
	if continued.OriginatorUserID != parseUUID(other) || continued.AccountableUserID != parseUUID(other) {
		t.Fatalf("callback used frozen anchor authority: %+v", continued)
	}
	if continued.DelegatedFromTaskID != second.ID {
		t.Fatal("callback audit lineage does not identify actual caller")
	}
	callbackToken := taskTokenFixture(t, uuidToString(continued.AgentID), uuidToString(continued.ID))
	for _, owner := range []string{other, testUserID} {
		target, _ := executableTarget(t, "callback-private-"+owner)
		dbfx.Exec(t, `UPDATE agent SET owner_id=$2,permission_mode='private' WHERE id=$1`, target, owner)
		title := "Callback invocation " + target
		bumpIssueCounter(t)
		w := serveAuthenticated(t, callbackToken, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
		if owner == other {
			if w.Code != http.StatusCreated {
				t.Fatalf("actual caller's private target denied: %d %s", w.Code, w.Body.String())
			}
			issue, _ := issueIDByTitle(t, title)
			cleanupDelegatedIssue(t, issue)
		} else if w.Code != http.StatusForbidden {
			t.Fatalf("callback borrowed frozen anchor's private rights: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestLegacyCallerProvenanceRemainsUnknown(t *testing.T) {
	source, review, _ := resultFixture(t, "legacy-callers")
	externalUpstream(t, source, "legacy-unproven-parent", true)
	dbfx.Exec(t, `UPDATE agent_task_delegation_return SET caller_task_ids=NULL WHERE task_id=$1`, review.ID)
	finishResultFixture(t, review)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET context=jsonb_set(context,'{delegation_result,upstream_returns_unknown}','false') WHERE id IN (SELECT notification_task_id FROM agent_task_delegation_return WHERE task_id=$1)`, review.ID)
	continued, claim := startResultHandlingTurn(t, review)
	if !claim.DelegationResult.UpstreamReturnsUnknown || continued.OriginatorUserID.Valid {
		t.Fatal("legacy callback guessed authority or hid unknown upstream provenance")
	}
	requireUpstreamReturns(t, continued)
	// A later downstream review must retain this uncertainty from DB evidence.
	secondReview, _ := callerReview(t, continued, review, "", "review legacy continuation")
	completeContinuation(t, continued, claim, "Legacy continuation awaits review")
	finishResultFixture(t, secondReview)
	next, nextClaim := startResultHandlingTurn(t, secondReview)
	if !nextClaim.DelegationResult.UpstreamReturnsUnknown || next.OriginatorUserID.Valid {
		t.Fatal("unknown ancestor became known after another hop")
	}
}

func TestIncompatibleCallerRefusedBeforeAcceptingWork(t *testing.T) {
	for _, mode := range []string{"scope", "authority"} {
		t.Run(mode, func(t *testing.T) {
			first, pending, _ := resultFixture(t, "refuse-caller-"+mode)
			overrides := testutil.Cols{}
			if mode == "scope" {
				overrides["issue_id"] = dbfx.Issue(t, "Different source scope")
			} else {
				other := dbfx.User(t, "Conflicting caller", "conflicting-caller@example.com")
				dbfx.Member(t, testWorkspaceID, other, "member")
				overrides["originator_user_id"], overrides["accountable_user_id"] = other, other
			}
			second := nextCaller(t, first, overrides)
			token := taskTokenFixture(t, uuidToString(second.AgentID), uuidToString(second.ID))
			code, _, body := intentComment(t, token, uuidToString(pending.IssueID), fmt.Sprintf("[@Review](mention://agent/%s) incompatible new request", uuidToString(pending.AgentID)), "")
			if code != http.StatusConflict {
				t.Fatalf("incompatible caller accepted: %d %s", code, body)
			}
			if countValue(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, pending.IssueID) != 0 {
				t.Fatal("refused caller left accepted comment")
			}
		})
	}
}

func TestLegacyMaterializedCallbackRefusesUnprovenAuthority(t *testing.T) {
	_, review, _ := resultFixture(t, "legacy-materialized-authority")
	finishResultFixture(t, review)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	turnID := queryString(t, `SELECT notification_task_id::text FROM agent_task_delegation_return WHERE task_id=$1`, review.ID)
	dbfx.Exec(t, `UPDATE agent_task_delegation_return SET caller_task_ids=NULL WHERE task_id=$1`, review.ID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, turnID, testUserID)
	turn, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(turnID))
	if err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newOwnerDaemonRequest("POST", "/claim", nil), "runtimeId", uuidToString(turn.RuntimeID))
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityDelegationResultsV1)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&response)
	if response.Task != nil {
		t.Fatal("unproven legacy authority reached provider admission")
	}
	refused, err := testHandler.Queries.GetAgentTask(context.Background(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refused.Status != "failed" || refused.FailureReason.String != protocol.DelegationResultUnavailable || refused.StartedAt.Valid {
		t.Fatalf("legacy callback was not refused before input: status=%s reason=%s started=%v", refused.Status, refused.FailureReason.String, refused.StartedAt.Valid)
	}
	if !strings.Contains(refused.Error.String, "result_caller_authority_changed") {
		t.Fatalf("missing actionable authority refusal: %s", refused.Error.String)
	}
}
