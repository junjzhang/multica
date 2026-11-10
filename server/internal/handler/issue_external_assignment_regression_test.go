package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExternalMixedMentions(t *testing.T) {
	for _, squadFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(squadFirst), func(t *testing.T) {
			agent, session, _ := externalAgentFixture(t, "Review mixed leader", "review-mixed")
			squad := dbfx.Squad(t, "Review mixed squad", agent)
			issue := dbfx.Issue(t, "Review mixed issue")
			bindExternalSession(t, agent, session, issue)
			a := fmt.Sprintf("[@Leader](mention://agent/%s)", agent)
			s := fmt.Sprintf("[@Squad](mention://squad/%s)", squad)
			content := a + " " + s + " please handle"
			if squadFirst {
				content = s + " " + a + " please handle"
			}
			w := httptest.NewRecorder()
			testHandler.CreateComment(w, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/comments", map[string]any{"content": content}), "id", issue))
			if w.Code != http.StatusCreated {
				t.Fatalf("mixed mentions status=%d body=%s", w.Code, w.Body.String())
			}
			if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1`, issue); n != 1 {
				t.Fatalf("got %d deliveries", n)
			}
		})
	}
}

func TestExternalSquadEditAfterBriefingChange(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Review edited leader", "review-edit")
	squad := dbfx.Squad(t, "Review edit squad", agent)
	issue := dbfx.Issue(t, "Review edit issue")
	bindExternalSession(t, agent, session, issue)
	content := fmt.Sprintf("[@Squad](mention://squad/%s) please handle", squad)
	var comment CommentResponse
	testutil.Call(t, testHandler.CreateComment, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/comments", map[string]any{"content": content}), "id", issue)).Want(http.StatusCreated).JSON(&comment)
	dbfx.Exec(t, `UPDATE squad SET instructions='new operating instructions' WHERE id=$1`, squad)
	w := httptest.NewRecorder()
	testHandler.UpdateComment(w, withURLParam(newRequest(http.MethodPut, "/api/comments/"+comment.ID, map[string]any{"content": content + " (clarified)"}), "commentId", comment.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("ordinary edit after squad update status=%d body=%s", w.Code, w.Body.String())
	}
}

type assignmentBeforeBegin struct {
	base   txStarter
	before func()
}

func (s assignmentBeforeBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	s.before()
	return s.base.Begin(ctx)
}

func TestExternalAssignmentSnapshotReturnRace(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, source := range []string{"external_missing", "external_valid", "managed_closed"} {
			t.Run(fmt.Sprintf("batch=%v/%s", batch, source), func(t *testing.T) {
				agent, session, _ := externalAgentFixture(t, "Race recipient", "race-recipient")
				caller, callerSession, _ := externalAgentFixture(t, "Race caller", "race-caller")
				issue := dbfx.Issue(t, "Race assignment", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})
				bindExternalSession(t, agent, session, issue)
				h := *testHandler
				h.TxStarter = assignmentBeforeBegin{base: h.TxStarter, before: func() {
					dbfx.Exec(t, `UPDATE issue SET assignee_type=NULL, assignee_id=NULL WHERE id=$1`, issue)
				}}
				updates := map[string]any{"assignee_type": "agent", "assignee_id": agent, "recipient_session_id": session}
				r := externalAgentRequest(t, caller, http.MethodPut, "/api/issues/"+issue, updates)
				if batch {
					r = externalAgentRequest(t, caller, http.MethodPost, "/api/issues/batch", map[string]any{"issue_ids": []string{issue}, "updates": updates})
				}
				if source == "external_valid" {
					var hostSession string
					if err := testPool.QueryRow(t.Context(), `SELECT session_id FROM external_agent_session WHERE id=$1`, callerSession).Scan(&hostSession); err != nil {
						t.Fatal(err)
					}
					r.Header.Set(ReturnSessionHeader, hostSession)
				}
				if source == "managed_closed" {
					caller = dbfx.Agent(t, "Closed managed dispatcher", testRuntimeID)
					scope := dbfx.Issue(t, "Closed dispatcher source")
					task := dbfx.Task(t, caller, testutil.Cols{"issue_id": scope, "runtime_id": testRuntimeID, "status": "completed"})
					r.Header.Set("X-Agent-ID", caller)
					r.Header.Set("X-Actor-Source", "task_token")
					r.Header.Set("X-Task-ID", task)
				}
				if source == "external_missing" {
					// Model a legacy credential that is not bound to any session:
					// without an explicit return session there is no provable
					// address, so the dispatch must write nothing.
					r.Header.Del(ActorSessionHeader)
				}
				w := httptest.NewRecorder()
				if batch {
					h.BatchUpdateIssues(w, r)
				} else {
					h.UpdateIssue(w, withURLParam(r, "id", issue))
				}
				mailboxes := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1`, issue)
				returns := countRows(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issue)
				if source == "external_valid" {
					if w.Code != http.StatusOK || mailboxes != 1 || returns != 1 {
						t.Fatalf("valid dispatch status=%d, mailbox=%d, returns=%d: %s", w.Code, mailboxes, returns, w.Body.String())
					}
				} else if mailboxes != 0 || returns != 0 {
					t.Fatalf("invalid dispatch leaked work: status=%d, mailbox=%d, returns=%d", w.Code, mailboxes, returns)
				}
			})
		}
	}
}

func TestExternalConcurrentTargetConflict(t *testing.T) {
	agent, first, owner := externalAgentFixture(t, "Review concurrent leader", "review-concurrent")
	second := dbfx.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agent, "owner_user_id": owner, "provider": "paseo", "host_id": "other", "session_id": "other", "daemon_id": "other"})
	issue := dbfx.Issue(t, "Review concurrent issue")
	start := make(chan struct{})
	responses := make(chan int, 2)
	for _, session := range []string{first, second} {
		go func(session string) {
			<-start
			w := httptest.NewRecorder()
			r := newRequest(http.MethodPut, "/api/issues/"+issue, map[string]any{"assignee_type": "agent", "assignee_id": agent, "recipient_session_id": session})
			testHandler.UpdateIssue(w, withURLParam(r, "id", issue))
			responses <- w.Code
		}(session)
	}
	close(start)
	counts := map[int]int{}
	for range 2 {
		counts[<-responses]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent responses: %v", counts)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1`, issue); n != 1 {
		t.Fatalf("concurrent deliveries: %d", n)
	}
}

func TestExternalBatchAssignments(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Review batch leader", "review-batch")
	squad := dbfx.Squad(t, "Review batch squad", agent)
	issues := []string{dbfx.Issue(t, "Review batch one"), dbfx.Issue(t, "Review batch two")}
	body := map[string]any{"issue_ids": issues, "updates": map[string]any{"assignee_type": "squad", "assignee_id": squad, "recipient_session_id": session}}
	testutil.Call(t, testHandler.BatchUpdateIssues, newRequest(http.MethodPost, "/api/issues/batch", body)).Want(http.StatusOK)
	for _, issue := range issues {
		if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1 AND target_session_id=$2`, issue, session); n != 1 {
			t.Fatalf("issue %s got %d exact deliveries", issue, n)
		}
	}
}
