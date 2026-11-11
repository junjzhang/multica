package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestTaskLeadershipOrdinaryWriteDoesNotBreakDispatch(t *testing.T) {
	for _, boundary := range []string{"claim", "start", "mailbox"} {
		t.Run(boundary, func(t *testing.T) {
			var issueID, squadID, agentID, taskID, owner, session string
			if boundary == "mailbox" {
				f := taskLeadershipFixture(t)
				issueID = f.create(t, f.tokenA)
				squadID = f.squad
				agentID = f.agent
				owner = f.owner
				session = f.a
			} else {
				agentID = dbfx.Agent(t, "Contended task leader "+boundary, testRuntimeID)
				squadID = dbfx.Squad(t, "Contended task squad "+boundary, agentID)
				issueID = dbfx.Issue(t, "Ordinary root write "+boundary, testutil.Cols{"assignee_type": "squad", "assignee_id": squadID})
				dbfx.Exec(t, `UPDATE issue SET leadership_root_issue_id=id,leadership_squad_id=$2,leader_agent_id=$3,leadership_revision=1 WHERE id=$1`, issueID, squadID, agentID)
				status := "queued"
				if boundary == "start" {
					status = "dispatched"
				}
				taskID = dbfx.Task(t, agentID, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issueID, "status": status, "is_leader_task": true, "squad_id": squadID})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
			if err != nil {
				t.Fatal(err)
			}
			squad, err := testHandler.Queries.GetSquad(ctx, parseUUID(squadID))
			if err != nil {
				t.Fatal(err)
			}
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `UPDATE issue SET title=title||' edited' WHERE id=$1`, issueID); err != nil {
				t.Fatal(err)
			}

			leader, err := issueleader.ResolveIssueLeader(ctx, testHandler.Queries, issue, squad)
			if err != nil || uuidToString(leader.Agent.ID) != agentID {
				t.Errorf("ordinary root write made leader unavailable: %+v %v", leader, err)
			}
			var preview CommentTriggerPreviewResponse
			testutil.Call(t, testHandler.PreviewCommentTriggers, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments/preview", map[string]any{"content": fmt.Sprintf("[@Squad](mention://squad/%s) evaluate", squadID)}).WithContext(ctx), "id", issueID)).Want(200).JSON(&preview)
			for _, blocked := range preview.Blocked {
				if blocked.ReasonCode == ReasonTargetUnavailable {
					t.Error("ordinary root write made @squad unavailable")
				}
			}

			type result struct {
				task   *db.AgentTaskQueue
				claims []delivery.Claim
				err    error
			}
			done := make(chan result, 1)
			go func() {
				var got result
				switch boundary {
				case "claim":
					got.task, got.err = testHandler.TaskService.ClaimTask(ctx, parseUUID(agentID))
				case "start":
					got.task, got.err = testHandler.TaskService.StartTask(ctx, parseUUID(taskID))
				case "mailbox":
					got.claims, got.err = delivery.NewQueue(testHandler.Queries, testPool).Claim(ctx, delivery.ClaimRequest{Scope: delivery.Scope{WorkspaceID: testWorkspaceID}, OwnerUserID: owner, DaemonID: "leadership-daemon", Limit: 1})
				}
				done <- got
			}()
			var got result
			finished := false
			select {
			case got = <-done:
				finished = true
			case <-time.After(100 * time.Millisecond):
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if !finished {
				select {
				case got = <-done:
				case <-ctx.Done():
					t.Fatal("dispatch did not resume after the ordinary write committed")
				}
			}
			if got.err != nil {
				t.Fatalf("ordinary root write failed %s: %v", boundary, got.err)
			}
			if boundary == "mailbox" {
				if len(got.claims) != 1 || got.claims[0].Target.SessionID != session {
					t.Fatalf("mailbox claim after ordinary write=%+v", got.claims)
				}
			} else if got.task == nil || uuidToString(got.task.ID) != taskID {
				t.Fatalf("%s did not preserve the queued task", boundary)
			}
		})
	}
}

func TestTaskLeadershipHandoffConflictsWithAClaimedQueueRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	old := dbfx.Agent(t, "Claim race old leader", testRuntimeID)
	next := dbfx.Agent(t, "Claim race new leader", testRuntimeID)
	squad := dbfx.Squad(t, "Claim race squad", old)
	dbfx.SquadMember(t, squad, "agent", next)
	issue := dbfx.Issue(t, "Claim race root", testutil.Cols{"assignee_type": "squad", "assignee_id": squad})
	dbfx.Exec(t, `UPDATE issue SET leadership_root_issue_id=id,leadership_squad_id=$2,leader_agent_id=$3,leadership_revision=1 WHERE id=$1`, issue, squad, old)
	queued := dbfx.Task(t, old, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issue, "status": "queued", "is_leader_task": true, "squad_id": squad})
	var revision int64
	dbfx.QueryRow(t, "SELECT revision FROM issue WHERE id=$1", issue).Scan(&revision)
	request := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedRevision: 1, ExpectedIssueRevision: revision, PreviousAgentID: old, AgentID: next, Reason: "Explicit handoff racing a queue claim"}
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), "SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE", queued); err != nil {
		t.Fatal(err)
	}
	path := "/api/issues/" + issue + "/leadership/handoff"
	testutil.Call(t, testHandler.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, path, request).WithContext(ctx), "id", issue)).Want(409)
	var leader string
	var leadershipRevision int64
	dbfx.QueryRow(t, "SELECT leader_agent_id,leadership_revision FROM issue WHERE id=$1", issue).Scan(&leader, &leadershipRevision)
	if leader != old || leadershipRevision != 1 {
		t.Fatal("a losing handoff changed leadership")
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, testHandler.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, path, request).WithContext(ctx), "id", issue)).Want(200)
	var status string
	dbfx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", queued).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("old queued task=%s", status)
	}
}

func TestTaskLeadershipHandoffKeepsAcceptedUpstreamOnReplacementRun(t *testing.T) {
	_, source, token := managedDispatcherFixture(t, "handoff-upstream")
	old, _ := executableTarget(t, "Original delegated leader")
	next, _ := executableTarget(t, "Replacement delegated leader")
	squad := dbfx.Squad(t, "Frozen upstream squad", old)
	dbfx.SquadMember(t, squad, "agent", next)
	body := createIssueBody("Frozen upstream across handoff", squad)
	body["assignee_type"] = "squad"
	created := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", body, nil, nil)
	if created.Code != 201 {
		t.Fatalf("create delegated root: %d %s", created.Code, created.Body.String())
	}
	var issue IssueResponse
	if err := json.Unmarshal(created.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	cleanupDelegatedIssue(t, issue.ID)
	accepted, _ := acceptedReturn(t, issue.ID)
	request := leadershipRequest{ClientRequestID: uuidToString(dbid.NewV7()), ExpectedRevision: 1, ExpectedIssueRevision: issue.Revision, PreviousAgentID: old, AgentID: next, Reason: "Preserve the accepted source while replacing the leader"}
	testutil.Call(t, testHandler.HandoffIssueLeadership, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue.ID+"/leadership/handoff", request), "id", issue.ID)).Want(200)
	var runID, returnID, sourceID string
	dbfx.QueryRow(t, `SELECT t.id,l.delegation_return_id,d.source_task_id FROM agent_task_queue t
 JOIN agent_task_delegation_return l ON l.task_id=t.id JOIN issue_delegation_return d ON d.id=l.delegation_return_id
 WHERE t.issue_id=$1 AND t.agent_id=$2`, issue.ID, next).Scan(&runID, &returnID, &sourceID)
	if returnID != uuidToString(accepted.ID) || sourceID != source {
		t.Fatalf("replacement upstream=%s/%s want=%s/%s", returnID, sourceID, uuidToString(accepted.ID), source)
	}
	var assignmentRun string
	dbfx.QueryRow(t, "SELECT task_id FROM issue_assignment_delegation WHERE issue_id=$1 AND assignee_type='squad' AND assignee_id=$2", issue.ID, squad).Scan(&assignmentRun)
	if assignmentRun != runID {
		t.Fatal("assignment intent still points to the retired run")
	}
}
