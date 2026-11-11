package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestContractExternalAssignmentKeepsRecipientAndAtomicWrites(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Contract external executor", "contract-assignment")
	otherSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agent, "owner_user_id": owner,
		"provider": "paseo", "host_id": "other-contract-host", "session_id": "other-contract-session", "daemon_id": "other-contract-daemon",
	})
	var issue IssueResponse
	testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title": "Contract with exact external recipient", "assignee_type": "agent", "assignee_id": agent,
		"recipient_session_id": session, "coordinator_type": "member", "coordinator_id": testUserID,
	})).Want(http.StatusCreated).JSON(&issue)
	cleanupDelegatedIssue(t, issue.ID)
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue.ID)
	var mailbox, target string
	dbfx.QueryRow(t, "SELECT id,target_session_id FROM agent_mailbox_message WHERE issue_id=$1", issue.ID).Scan(&mailbox, &target)
	if target != session {
		t.Fatalf("assignment targeted %s, want %s", target, session)
	}
	contractCall(t, issue.ID, definition(0), "").Want(http.StatusOK)
	// A refused recipient change must not partially update the contracted issue.
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue.ID, map[string]any{
		"assignee_type": "agent", "assignee_id": agent, "recipient_session_id": otherSession, "title": "must roll back",
	}), "id", issue.ID)).Want(http.StatusConflict)
	var title string
	dbfx.QueryRow(t, "SELECT title FROM issue WHERE id=$1", issue.ID).Scan(&title)
	if title != issue.Title {
		t.Fatalf("recipient conflict changed title to %q", title)
	}
	var view ContractView
	testutil.Call(t, testHandler.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/contract", nil), "id", issue.ID)).Want(http.StatusOK).JSON(&view)
	if view.Stale || view.Contract.Sequence != 1 {
		t.Fatalf("refused recipient change affected agreement: %+v", view)
	}
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "External recipient delivery"}
	testutil.Call(t, testHandler.UpdateIssueContract, withURLParam(externalAgentRequest(t, agent, http.MethodPost, "/api/issues/"+issue.ID+"/contract", submit), "id", issue.ID)).Want(http.StatusOK)
	accept := contractRequest{Action: "accept", Version: 1, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0, 1}}
	contractCall(t, issue.ID, accept, "").Want(http.StatusOK).JSON(&view)
	var status, storedExecutor, storedTarget string
	dbfx.QueryRow(t, "SELECT status,assignee_id FROM issue WHERE id=$1", issue.ID).Scan(&status, &storedExecutor)
	dbfx.QueryRow(t, "SELECT target_session_id FROM agent_mailbox_message WHERE id=$1", mailbox).Scan(&storedTarget)
	if status != "done" || storedExecutor != agent || storedTarget != session || view.PendingActor != nil || len(view.Accepted) != 2 {
		t.Fatalf("acceptance changed assignment or lost completion: %s %s %s %+v", status, storedExecutor, storedTarget, view)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_mailbox_message WHERE issue_id=$1", issue.ID); n != 1 {
		t.Fatalf("contract lifecycle duplicated external assignment: %d messages", n)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", issue.ID); n != 0 {
		t.Fatalf("external contract started %d managed tasks", n)
	}
}

func TestLegacyExternalContractContinuesWithoutRewritingBasis(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Legacy agreement executor", "legacy-agreement")
	issue := dbfx.Issue(t, "Legacy agreement", testutil.Cols{"assignee_type": "agent", "assignee_id": agent, "coordinator_type": "member", "coordinator_id": testUserID})
	bindExternalSession(t, agent, session, issue)
	dbfx.Exec(t, `UPDATE issue SET acceptance_criteria='["Legacy criterion"]'::jsonb WHERE id=$1`, issue)
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	dbfx.Exec(t, `INSERT INTO issue_contract(issue_id,workspace_id,data)
 SELECT id,workspace_id,jsonb_build_object('sequence',1,'versions',jsonb_build_array(jsonb_build_object('number',1,'goal','Legacy goal','scope','','authorization_note','Legacy note','criteria',jsonb_build_array('Legacy criterion'),'executor',jsonb_build_object('type','agent','id',assignee_id),'acceptor',jsonb_build_object('type','member','id',coordinator_id),'basis',issue_contract_basis(issue),'author',jsonb_build_object('type','member','id',coordinator_id),'created_at',now())),'deliveries','[]'::jsonb,'decisions','[]'::jsonb,'requests','[]'::jsonb) FROM issue WHERE id=$1`, issue)
	token := externalSessionTokenFixture(t, agent, session, owner, time.Now().Add(time.Hour))
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Legacy executor evidence"}
	var view ContractView
	authenticatedContractCall(t, issue, submit, token).Want(200).JSON(&view)
	if view.Stale || view.Contract.Pending().Author.SessionID != session || view.Contract.Current().Author.SessionID != "" {
		t.Fatal("legacy continuation changed basis or historical authors")
	}
	var raw string
	dbfx.QueryRow(t, "SELECT data->'versions'->0->'basis' FROM issue_contract WHERE issue_id=$1", issue).Scan(&raw)
	if strings.Contains(raw, "session_id") || strings.Contains(raw, "leadership") {
		t.Fatal("legacy basis was rewritten")
	}
}
