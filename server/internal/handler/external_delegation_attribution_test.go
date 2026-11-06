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

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExternalDelegationPreservesAuthorityAcrossAgentHop(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	dispatcher, sessionID, owner := externalAgentFixture(t, "External authority", "delegation-daemon")
	token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	worker, workerRuntime := executableTarget(t, "external authority worker")
	reviewer, reviewerRuntime := executableTarget(t, "external authority reviewer")
	for _, id := range []string{workerRuntime, reviewerRuntime} {
		dbfx.Exec(t, `UPDATE agent_runtime SET visibility='public' WHERE id=$1`, id)
	}
	for _, id := range []string{worker, reviewer} {
		dbfx.Exec(t, `UPDATE agent SET owner_id=$2,permission_mode='private' WHERE id=$1`, id, owner)
	}
	bumpIssueCounter(t)
	title := fmt.Sprintf("External authority %d", time.Now().UnixNano())
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, worker), nil, func(r *http.Request) {
		r.Header.Set(ReturnSessionHeader, sessionID)
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("missing delegated issue")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	taskID := latestTaskIDFor(t, issueID, worker)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',session_id=$2,work_dir=$3,
		resource_control_required=true,session_pin_confirmed=true,session_pin_generation=gen_random_uuid()
		WHERE id=$1`, taskID, "authority-worker-session", "/work/authority-worker")
	workerToken := taskTokenFixture(t, worker, taskID)
	w = serveAuthenticated(t, workerToken, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": fmt.Sprintf("[@Reviewer](mention://agent/%s) Review this change.", reviewer),
	}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("review request status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.TriggerOutcomes) != 1 || response.TriggerOutcomes[0].Status != DispatchQueued {
		t.Fatalf("second hop did not enqueue private reviewer: %s", w.Body.String())
	}
	for _, id := range []string{taskID, latestTaskIDFor(t, issueID, reviewer)} {
		task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		if uuidToString(task.OriginatorUserID) != owner || uuidToString(task.AccountableUserID) != owner || task.OriginatorSource.String != "delegation" {
			t.Fatalf("task %s lost external authority: originator=%s accountable=%s source=%s", id,
				uuidToString(task.OriginatorUserID), uuidToString(task.AccountableUserID), task.OriginatorSource.String)
		}
	}
}

func externalPrivateTarget(t *testing.T, owner, label string) string {
	t.Helper()
	agentID, runtimeID := executableTarget(t, label)
	dbfx.Exec(t, `UPDATE agent SET owner_id=$2,permission_mode='private' WHERE id=$1`, agentID, owner)
	dbfx.Exec(t, `UPDATE agent_runtime SET visibility='public' WHERE id=$1`, runtimeID)
	return agentID
}

func TestExternalAssignmentAndRerunPreserveAuthority(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	dispatcher, sessionID, owner := externalAgentFixture(t, "External assignment", "delegation-daemon")
	token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	worker := externalPrivateTarget(t, owner, "external assignment worker")
	issueID := dbfx.Issue(t, "External assignment", testutil.Cols{"creator_type": "agent", "creator_id": dispatcher})
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	setSession := func(r *http.Request) { r.Header.Set(ReturnSessionHeader, sessionID) }
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent", "assignee_id": worker, "status": "todo",
	}, map[string]string{"id": issueID}, setSession)
	if w.Code != http.StatusOK {
		t.Fatalf("assign = %d: %s", w.Code, w.Body.String())
	}
	first := latestTaskIDFor(t, issueID, worker)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, first)
	w = serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost, "/api/issues/"+issueID+"/rerun", map[string]any{
		"task_id": first,
	}, map[string]string{"id": issueID}, setSession)
	if w.Code != http.StatusAccepted {
		t.Fatalf("rerun = %d: %s", w.Code, w.Body.String())
	}
	for _, id := range []string{first, latestTaskIDFor(t, issueID, worker)} {
		task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(id))
		if err != nil || uuidToString(task.OriginatorUserID) != owner || task.OriginatorSource.String != "delegation" {
			t.Fatalf("task %s originator=%s source=%s err=%v", id, uuidToString(task.OriginatorUserID), task.OriginatorSource.String, err)
		}
	}
}

func TestExternalCommentAuthoritySurvivesRecoveryAndRespectsRevocation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, mode := range []string{"recover", "owner_removed", "author_archived", "target_revoked", "legacy", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			dispatcher, sessionID, owner := externalAgentFixture(t, "External recovery "+mode, "delegation-daemon")
			token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
			target := externalPrivateTarget(t, owner, "external recovery "+mode)
			issueID := dbfx.Issue(t, "External comment recovery "+mode)
			t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
			drop := failCommentTaskInsertFor(t, target, "external_"+mode)
			w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
				"content":                     fmt.Sprintf("[@Worker](mention://agent/%s) Work on this.", target),
				"external_originator_user_id": testUserID,
			}, map[string]string{"id": issueID}, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, sessionID) })
			drop()
			if w.Code != http.StatusCreated {
				t.Fatalf("save = %d: %s", w.Code, w.Body.String())
			}
			var out struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			comment, err := testHandler.Queries.GetComment(t.Context(), parseUUID(out.ID))
			if err != nil || uuidToString(comment.ExternalOriginatorUserID) != owner || comment.SourceTaskID.Valid {
				t.Fatalf("stored authoring evidence = %s, source=%s, err=%v", uuidToString(comment.ExternalOriginatorUserID), uuidToString(comment.SourceTaskID), err)
			}
			if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND status='pending'`, out.ID); n != 1 {
				t.Fatalf("pending obligations = %d", n)
			}
			switch mode {
			case "owner_removed":
				dbfx.Exec(t, `DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, owner)
			case "author_archived":
				dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, dispatcher)
			case "target_revoked":
				dbfx.Exec(t, `UPDATE agent SET owner_id=$2 WHERE id=$1`, target, testUserID)
			case "legacy":
				dbfx.Exec(t, `UPDATE comment SET external_originator_user_id=NULL WHERE id=$1`, out.ID)
			case "deleted":
				deleted := serveAuthenticated(t, token, http.HandlerFunc(testHandler.DeleteComment), http.MethodDelete, "/api/comments/"+out.ID, nil, map[string]string{"commentId": out.ID}, nil)
				if deleted.Code != http.StatusNoContent && deleted.Code != http.StatusOK {
					t.Fatalf("delete = %d: %s", deleted.Code, deleted.Body.String())
				}
			}
			if _, err := restartedHandler().ConsumeCommentDispatchObligations(t.Context(), 50); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "recover" {
				want = 1
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, target, out.ID); n != want {
				t.Fatalf("runs = %d, want %d", n, want)
			}
			if mode != "recover" {
				return
			}
			task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(latestTaskIDFor(t, issueID, target)))
			if err != nil || uuidToString(task.OriginatorUserID) != owner || task.OriginatorSource.String != "delegation" {
				t.Fatalf("recovered run lost principal: %s/%s %v", uuidToString(task.OriginatorUserID), task.OriginatorSource.String, err)
			}
		})
	}
}

func TestExternalCommentEditRestampsOrClearsAuthority(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	dispatcher, sessionID, owner := externalAgentFixture(t, "External edit", "delegation-daemon")
	token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	target := externalPrivateTarget(t, owner, "external edit target")
	issueID := dbfx.Issue(t, "External edit provenance")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	code, commentID, body := intentComment(t, token, issueID, "Initial note", "")
	if code != http.StatusCreated {
		t.Fatalf("create = %d: %s", code, body)
	}
	text := fmt.Sprintf("[@Worker](mention://agent/%s) Work after edit.", target)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch, "/api/comments/"+commentID, map[string]any{
		"content": text,
	}, map[string]string{"commentId": commentID}, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, sessionID) })
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Fatalf("author edit = %d: %s", w.Code, w.Body.String())
	}
	// An administrator may edit another actor's text, not lend that actor's authority.
	r := withURLParam(newRequest(http.MethodPatch, "/api/comments/"+commentID, map[string]any{"content": "Edited by administrator"}), "commentId", commentID)
	testutil.Call(t, testHandler.UpdateComment, r).Want(http.StatusOK)
	comment, err := testHandler.Queries.GetComment(t.Context(), parseUUID(commentID))
	if err != nil || comment.ExternalOriginatorUserID.Valid || comment.SourceTaskID.Valid {
		t.Fatalf("admin edit retained authorization: external=%s task=%s err=%v", uuidToString(comment.ExternalOriginatorUserID), uuidToString(comment.SourceTaskID), err)
	}
}

func TestExternalAssignmentCannotBorrowPendingRunAuthority(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	dispatcher, sessionID, owner := externalAgentFixture(t, "External coalesce", "delegation-daemon")
	token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	worker, runtimeID := executableTarget(t, "external coalesce target")
	issueID := dbfx.Issue(t, "Existing run with another originator")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	prior := dbfx.Task(t, worker, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "queued",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent", "assignee_id": worker,
	}, map[string]string{"id": issueID}, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, sessionID) })
	if w.Code != http.StatusConflict {
		t.Fatalf("assignment = %d: %s", w.Code, w.Body.String())
	}
	if got := latestTaskIDFor(t, issueID, worker); got != prior {
		t.Fatalf("pending run replaced: %s", got)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issueID); n != 0 {
		t.Fatalf("refused assignment recorded %d returns", n)
	}
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
	if err != nil || issue.AssigneeID.Valid {
		t.Fatalf("refused assignment changed the issue: %s %v", uuidToString(issue.AssigneeID), err)
	}
}

func TestManagedCommentCannotForgeExternalOriginator(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	worker, sourceID, token := managedDispatcherFixture(t, "forged-external-authority")
	target := externalPrivateTarget(t, testUserID, "forged external private target")
	issueID := dbfx.Issue(t, "Managed requests cannot become external")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content":                     fmt.Sprintf("[@Worker](mention://agent/%s) Unauthorized request", target),
		"external_originator_user_id": testUserID,
	}, map[string]string{"id": issueID}, func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "external_agent")
		r.Header.Set("X-User-ID", testUserID)
	})
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "invocation_not_allowed") {
		t.Fatalf("forged request = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	comment, err := testHandler.Queries.GetComment(t.Context(), parseUUID(out.ID))
	if err != nil || comment.ExternalOriginatorUserID.Valid || uuidToString(comment.SourceTaskID) != sourceID {
		t.Fatalf("forged authority persisted: %s/%s %v", uuidToString(comment.ExternalOriginatorUserID), uuidToString(comment.SourceTaskID), err)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND issue_id=$2`, target, issueID); n != 0 {
		t.Fatalf("forged principal created %d runs", n)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, sourceID, testUserID)
	w = serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent", "assignee_id": target,
	}, map[string]string{"id": issueID}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("terminal %s borrowed authority: %d %s", worker, w.Code, w.Body.String())
	}
}

func TestExternalPrincipalCannotEscapeWorkspaceOrOutliveRevocation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, mode := range []string{"wrong_workspace", "token_revoked", "membership_removed"} {
		t.Run(mode, func(t *testing.T) {
			dispatcher, sessionID, owner := externalAgentFixture(t, "External scope "+mode, "delegation-daemon")
			token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
			target := externalPrivateTarget(t, owner, "external scope "+mode)
			workspaceID := testWorkspaceID
			switch mode {
			case "wrong_workspace":
				workspaceID = dbfx.Workspace(t, "Other workspace", fmt.Sprintf("other-%d", time.Now().UnixNano()))
				dbfx.Member(t, workspaceID, owner, "member")
			case "token_revoked":
				dbfx.Exec(t, `UPDATE external_agent_token SET revoked_at=now() WHERE agent_id=$1`, dispatcher)
			case "membership_removed":
				dbfx.Exec(t, `DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, owner)
			}
			title := fmt.Sprintf("Refused external scope %s %d", mode, time.Now().UnixNano())
			w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, func(r *http.Request) {
				r.Header.Set(ReturnSessionHeader, sessionID)
				r.Header.Set("X-Workspace-ID", workspaceID)
			})
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Fatalf("untrusted scope = %d: %s", w.Code, w.Body.String())
			}
			if _, exists := issueIDByTitle(t, title); exists {
				t.Fatal("rejected principal created an issue")
			}
		})
	}
}

func TestAssignmentCoalescesOnlyMatchingInvocationPrincipal(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, actor := range []string{"member", "managed", "unattributed"} {
		for _, matches := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/matches=%v", actor, matches), func(t *testing.T) {
				target, runtimeID := executableTarget(t, "assignment authority "+actor)
				issueID := dbfx.Issue(t, "Assignment coalescing "+actor)
				t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
				var originator any = testUserID
				if actor == "unattributed" {
					originator = nil
				}
				pendingOriginator := originator
				if !matches {
					pendingOriginator = dbfx.User(t, "Other originator", fmt.Sprintf("other-originator-%d@example.test", time.Now().UnixNano()))
				}
				pending := dbfx.Task(t, target, testutil.Cols{
					"issue_id": issueID, "runtime_id": runtimeID, "status": "queued",
					"originator_user_id": pendingOriginator, "accountable_user_id": pendingOriginator,
				})
				body := map[string]any{"assignee_type": "agent", "assignee_id": target}
				var w *httptest.ResponseRecorder
				if actor == "member" {
					w = httptest.NewRecorder()
					testHandler.UpdateIssue(w, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, body), "id", issueID))
				} else {
					_, source, token := managedDispatcherFixture(t, fmt.Sprintf("assignment-%s-%v", actor, matches))
					dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, source, originator)
					w = serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPut, "/api/issues/"+issueID, body, map[string]string{"id": issueID}, nil)
				}
				want := http.StatusConflict
				if matches {
					want = http.StatusOK
				}
				if w.Code != want {
					t.Fatalf("assignment = %d, want %d: %s", w.Code, want, w.Body.String())
				}
				if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, target); n != 1 {
					t.Fatalf("assignment changed pending run count to %d", n)
				}
				if got := latestTaskIDFor(t, issueID, target); got != pending {
					t.Fatalf("assignment replaced original run %s with %s", pending, got)
				}
			})
		}
	}
}
