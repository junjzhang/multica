package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestExternalSquadAssignmentAtomicTarget(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Assignment leader", "assignment-daemon")
	otherSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agent, "owner_user_id": owner,
		"provider": "paseo", "host_id": "second-host", "session_id": "second-session", "daemon_id": "second-daemon",
	})
	squad := dbfx.Squad(t, "External assignment squad", agent, testutil.Cols{"instructions": "Use the isolated test repository."})
	member := dbfx.Agent(t, "Do not start this member", testRuntimeID)
	dbfx.SquadMember(t, squad, "agent", member)
	create := map[string]any{"title": "External squad atomic assignment", "assignee_type": "squad", "assignee_id": squad}
	response := testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", create)).Want(http.StatusBadRequest)
	if !strings.Contains(response.Body.String(), "default_leader_must_be_managed") {
		t.Fatal(response.Body.String())
	}
	if n := countRows(t, `SELECT count(*) FROM issue WHERE title=$1 AND workspace_id=$2`, create["title"], testWorkspaceID); n != 0 {
		t.Fatalf("failed create left %d issues", n)
	}
	create["recipient_session_id"] = session
	var issue IssueResponse
	testutil.Call(t, testHandler.CreateIssue, externalAgentRequest(t, agent, http.MethodPost, "/api/issues", create)).Want(http.StatusCreated).JSON(&issue)
	cleanupDelegatedIssue(t, issue.ID)
	var storedTarget, mailboxID string
	var payload []byte
	if err := testPool.QueryRow(t.Context(), `SELECT id, target_session_id, payload FROM agent_mailbox_message WHERE issue_id=$1`, issue.ID).Scan(&mailboxID, &storedTarget, &payload); err != nil {
		t.Fatal(err)
	}
	if storedTarget != session {
		t.Fatalf("target=%s want explicit session %s", storedTarget, session)
	}
	var request protocol.ExternalIssueRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	if request.SquadID != squad || !strings.Contains(request.Briefing, "Use the isolated test repository.") || !strings.Contains(request.Briefing, member) {
		t.Fatalf("missing leader context: %+v", request)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issue.ID); n != 0 {
		t.Fatalf("external assignment started %d managed tasks", n)
	}

	// A conflicting explicit session rolls back the ordinary issue patch too.
	updates := map[string]any{"assignee_type": "squad", "assignee_id": squad, "recipient_session_id": otherSession, "title": "must roll back"}
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue.ID, updates), "id", issue.ID)).Want(http.StatusConflict)
	stored, err := testHandler.Queries.GetIssueInWorkspace(t.Context(), db.GetIssueInWorkspaceParams{ID: parseUUID(issue.ID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil || stored.Title != create["title"] {
		t.Fatalf("conflict leaked update: %+v %v", stored, err)
	}

	// Re-observing exactly the same event preserves its stable Mailbox ID.
	target, err := service.ResolveAssignmentTarget(t.Context(), testHandler.Queries, stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ExternalAssignment(context.Background(), testHandler.Queries, stored, target, nil, true, "agent", agent, owner, ""); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1`, issue.ID); n != 1 {
		t.Fatalf("replayed assignment created %d messages", n)
	}

	// Evaluation uses delivered Mailbox proof, never a fabricated task.
	evaluation := map[string]any{"outcome": "no_action", "reason": "isolated evaluation", "mailbox_id": mailboxID}
	evalReq := func() *http.Request {
		return withURLParam(externalAgentRequest(t, agent, http.MethodPost, "/api/issues/"+issue.ID+"/squad-evaluated", evaluation), "id", issue.ID)
	}
	testutil.Call(t, testHandler.RecordSquadLeaderEvaluation, evalReq()).Want(http.StatusBadRequest)
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET delivery_status='delivered', delivered_at=now() WHERE id=$1`, mailboxID)
	testutil.Call(t, testHandler.RecordSquadLeaderEvaluation, evalReq()).Want(http.StatusCreated)
	dbfx.Exec(t, `UPDATE squad SET leader_id=$1 WHERE id=$2`, member, squad)
	testutil.Call(t, testHandler.RecordSquadLeaderEvaluation, evalReq()).Want(http.StatusCreated)
	var frozen string
	if err := testPool.QueryRow(t.Context(), `SELECT target_session_id FROM agent_mailbox_message WHERE id=$1`, mailboxID).Scan(&frozen); err != nil || frozen != session {
		t.Fatalf("leader rotation retargeted history: %s %v", frozen, err)
	}
}

func TestExternalAssignmentParkingAndReassignment(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Parked assignment", "parked-assignment-daemon")
	squad := dbfx.Squad(t, "Parked external squad", agent)
	recipient, recipientSession, _ := externalAgentFixture(t, "Unbound direct recipient", "unbound-direct-recipient")
	for _, parked := range []string{"backlog", "suppress"} {
		t.Run(parked, func(t *testing.T) {
			issue := dbfx.Issue(t, "parked "+parked)
			bindTaskLeadership(t, issue, squad, agent, session)
			updates := map[string]any{"assignee_type": "squad", "assignee_id": squad}
			if parked == "backlog" {
				updates["status"] = "backlog"
			} else {
				updates["suppress_run"] = true
			}
			testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, updates), "id", issue)).Want(http.StatusOK)
			if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1`, issue); n != 0 {
				t.Fatalf("parked assignment sent %d messages", n)
			}
			// Status changes alone never start work.
			testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, map[string]any{"status": "todo"}), "id", issue)).Want(http.StatusOK)
			// Reassigning to the external agent requires a target; rejected writes
			// keep the squad ownership and do not leave a partial binding.
			updates = map[string]any{"assignee_type": "agent", "assignee_id": recipient}
			testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, updates), "id", issue)).Want(http.StatusBadRequest)
			updates["recipient_session_id"] = recipientSession
			testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, updates), "id", issue)).Want(http.StatusOK)
			if n := countRows(t, `SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1`, issue); n != 1 {
				t.Fatalf("explicit reassignment sent %d messages", n)
			}
		})
	}
}

func TestExternalAssignmentRejectsWrongRecipientAndManagedInput(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Recipient agent", "recipient-daemon")
	_, foreign, _ := externalAgentFixture(t, "Different recipient", "foreign-recipient-daemon")
	managed := dbfx.Agent(t, "Managed recipient", testRuntimeID)
	for _, id := range []string{agent, managed} {
		body := map[string]any{"title": "invalid recipient " + id, "assignee_type": "agent", "assignee_id": id, "recipient_session_id": foreign}
		testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", body)).Want(http.StatusBadRequest)
		if n := countRows(t, `SELECT count(*) FROM issue WHERE title=$1 AND workspace_id=$2`, body["title"], testWorkspaceID); n != 0 {
			t.Fatalf("invalid target left %d issues", n)
		}
	}
}
