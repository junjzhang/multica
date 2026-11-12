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

func TestCommentNotifyMixedReturnsAndRecovery(t *testing.T) {
	_, _, token := managedDispatcherFixture(t, "notify-mixed")
	a, _ := executableTarget(t, "notify-mixed-a")
	b, _ := executableTarget(t, "notify-mixed-b")
	issue := dbfx.Issue(t, "Notify mixed returns")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
	drop := failCommentTaskInsertFor(t, b, "notify_mixed")
	code, comment, body := intentComment(t, token, issue, fmt.Sprintf("[@A](mention://agent/%s) [@B](mention://agent-notify/%s)", a, b), "")
	drop()
	if code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, body)
	}
	if !strings.Contains(body, `"mode":"notify"`) || !strings.Contains(body, `"mode":"delegate"`) {
		t.Fatalf("mixed outcome modes: %s", body)
	}
	aTask := latestTaskIDFor(t, issue, a)
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, aTask); n != 1 {
		t.Fatalf("delegated returns = %d", n)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND agent_id=$2 AND delegation_return_id IS NULL AND status='pending'`, comment, b); n != 1 {
		t.Fatalf("pending notification = %d", n)
	}
	if _, err := restartedHandler().ConsumeCommentDispatchObligations(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	bTask := latestTaskIDFor(t, issue, b)
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, bTask); n != 0 {
		t.Fatalf("notification returns = %d", n)
	}
}

func TestCommentNotifySameTargetDelegates(t *testing.T) {
	_, _, token := managedDispatcherFixture(t, "notify-same")
	target, _ := executableTarget(t, "notify-same-target")
	issue := dbfx.Issue(t, "Notify and delegate same target")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
	code, comment, body := intentComment(t, token, issue, fmt.Sprintf("[@T](mention://agent-notify/%s) [@T](mention://agent/%s)", target, target), "")
	if code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, body)
	}
	task := latestTaskIDFor(t, issue, target)
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, task); n != 1 {
		t.Fatalf("returns = %d", n)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND agent_id=$2`, comment, target); n != 1 {
		t.Fatalf("obligations = %d", n)
	}
}

func TestCommentNotifyFoldDoesNotAddReturn(t *testing.T) {
	_, _, firstToken := managedDispatcherFixture(t, "notify-fold-first")
	_, _, secondToken := managedDispatcherFixture(t, "notify-fold-second")
	target, _ := executableTarget(t, "notify-fold-target")
	issue := dbfx.Issue(t, "Notification fold")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
	code, parent, body := intentComment(t, firstToken, issue, fmt.Sprintf("[@T](mention://agent/%s)", target), "")
	if code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, body)
	}
	task := latestTaskIDFor(t, issue, target)
	code, _, body = intentComment(t, secondToken, issue, fmt.Sprintf("[@T](mention://agent-notify/%s)", target), parent)
	if code != http.StatusCreated {
		t.Fatalf("notification: %d %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, task); n != 1 {
		t.Fatalf("returns after fold = %d", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issue); n != 1 {
		t.Fatalf("issue returns = %d", n)
	}
}

func TestCommentNotifyExternalMailbox(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Notify mailbox", "daemon-notify")
	issue := dbfx.Issue(t, "Notification mailbox")
	bindExternalSession(t, agent, session, issue)
	w := httptest.NewRecorder()
	req := testutil.WithURLParams(newRequest(http.MethodPost, "/api/issues/"+issue+"/comments", map[string]any{"content": fmt.Sprintf("[@T](mention://agent-notify/%s)", agent)}), "id", issue)
	testHandler.CreateComment(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		ID       string                  `json:"id"`
		Outcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Outcomes) != 1 || response.Outcomes[0].Mode != "notify" {
		t.Fatalf("outcomes: %+v", response.Outcomes)
	}
	var mode string
	if err := testPool.QueryRow(t.Context(), `SELECT payload->>'mode' FROM agent_mailbox_message WHERE comment_id=$1 AND recipient_agent_id=$2`, response.ID, agent).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "notify" {
		t.Fatalf("mailbox mode = %q", mode)
	}
}

func TestCommentNotifyEditChangesIntent(t *testing.T) {
	_, _, token := managedDispatcherFixture(t, "notify-edit")
	target, _ := executableTarget(t, "notify-edit-target")
	issue := dbfx.Issue(t, "Notification edit")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
	code, comment, body := intentComment(t, token, issue, fmt.Sprintf("[@T](mention://agent-notify/%s)", target), "")
	if code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, body)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issue); n != 0 {
		t.Fatalf("initial returns = %d", n)
	}
	edit := func(content string) {
		w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateComment), http.MethodPatch, "/api/workspaces/"+testWorkspaceID+"/comments/"+comment, map[string]any{"content": content}, map[string]string{"commentId": comment}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("edit: %d %s", w.Code, w.Body.String())
		}
	}
	edit(fmt.Sprintf("[@T](mention://agent/%s)", target))
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND delegation_return_id IS NOT NULL`, comment); n != 1 {
		t.Fatalf("delegated intent = %d", n)
	}
	edit(fmt.Sprintf("[@T](mention://agent-notify/%s) again", target))
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND delegation_return_id IS NULL`, comment); n != 1 {
		t.Fatalf("notification intent = %d", n)
	}
}

func TestCommentNotifyNotDelegatedInSessionTasks(t *testing.T) {
	dispatcher, session, owner := externalAgentFixture(t, "Notify projection", "notify-projection-daemon")
	token := externalSessionTokenFixture(t, dispatcher, session, owner, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "notify-projection-target")
	issue := dbfx.Issue(t, "Notification projection")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
	code, _, body := intentComment(t, token, issue, fmt.Sprintf("[@T](mention://agent-notify/%s)", target), "")
	if code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, body)
	}
	status, projection := getSessionTasks(t, sessionTasksRequest(owner, dispatcher, true, session))
	if status != http.StatusOK {
		t.Fatalf("session tasks status = %d", status)
	}
	for _, item := range projection.Delegated {
		if item.IssueID == issue {
			t.Fatal("notification appears as delegated")
		}
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issue); n != 0 {
		t.Fatalf("returns = %d", n)
	}
}

func TestCommentNotifySquadLeaderCoalescesAsDelegation(t *testing.T) {
	fx := newSquadCommentTriggerFixture(t)
	_, _, token := managedDispatcherFixture(t, "notify-squad")
	issue := uuidToString(fx.Issue.ID)
	code, comment, body := intentComment(t, token, issue, fmt.Sprintf("[@Leader](mention://agent-notify/%s) [@Squad](mention://squad/%s)", fx.LeaderID, fx.SquadID), "")
	if code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, body)
	}
	task := latestTaskIDFor(t, issue, fx.LeaderID)
	if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, task); n != 1 {
		t.Fatalf("returns = %d", n)
	}
	if n := countValue(t, `SELECT count(*) FROM comment_dispatch_obligation WHERE comment_id=$1 AND agent_id=$2 AND squad_id=$3 AND delegation_return_id IS NOT NULL`, comment, fx.LeaderID, fx.SquadID); n != 1 {
		t.Fatalf("squad obligation = %d", n)
	}
}

func TestCommentNotifyMixedFreshAndFold(t *testing.T) {
	for _, fold := range []bool{false, true} {
		name := "fresh"
		if fold {
			name = "fold"
		}
		t.Run(name, func(t *testing.T) {
			_, _, token := managedDispatcherFixture(t, "notify-mixed-"+name)
			a, _ := executableTarget(t, "notify-mixed-a-"+name)
			b, _ := executableTarget(t, "notify-mixed-b-"+name)
			issue := dbfx.Issue(t, "Mixed notification "+name)
			t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
			parent := ""
			var bTask string
			if fold {
				_, _, firstToken := managedDispatcherFixture(t, "notify-fold-prior")
				code, id, body := intentComment(t, firstToken, issue, fmt.Sprintf("[@B](mention://agent/%s)", b), "")
				if code != http.StatusCreated {
					t.Fatalf("first: %d %s", code, body)
				}
				parent = id
				bTask = latestTaskIDFor(t, issue, b)
			}
			code, mixedComment, body := intentComment(t, token, issue, fmt.Sprintf("[@A](mention://agent/%s) [@B](mention://agent-notify/%s)", a, b), parent)
			if code != http.StatusCreated {
				t.Fatalf("mixed: %d %s", code, body)
			}
			if !fold {
				bTask = latestTaskIDFor(t, issue, b)
			} else if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND trigger_comment_id=$2`, bTask, mixedComment); n != 1 {
				t.Fatal("notification did not fold into the pending run")
			}
			expected := 0
			if fold {
				expected = 1
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, bTask); n != expected {
				t.Fatalf("notification target returns = %d, want %d", n, expected)
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, latestTaskIDFor(t, issue, a)); n != 1 {
				t.Fatalf("delegated target returns = %d", n)
			}
		})
	}
}
