package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func authenticatedContractCall(t *testing.T, issue string, body contractRequest, token string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/contract", body), "id", issue)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	h := middleware.Auth(testHandler.Queries, nil, nil, nil)(http.HandlerFunc(testHandler.UpdateIssueContract))
	return testutil.Call(t, h.ServeHTTP, req)
}

func TestContractExternalOriginalRequesterMaintainsNotesWithoutGrants(t *testing.T) {
	issue, _ := contractFixture(t)
	creator, _, owner := externalAgentFixture(t, "Contract Original", "contract-original")
	token := externalTokenFixture(t, creator, owner, time.Now().Add(time.Hour))
	dbfx.Exec(t, "UPDATE issue SET creator_type='agent',creator_id=$2 WHERE id=$1", issue, creator)
	first := definition(0)
	first.AuthorizationNote = "Merge and deployment are described here, but this note grants neither."
	var view ContractView
	authenticatedContractCall(t, issue, first, token).Want(200).JSON(&view)
	if view.Contract.Current().Author.ID != creator || !view.CanEdit || view.MergeAuthorization.Allowed {
		t.Fatalf("external author or grant projection: %+v", view)
	}
	authenticatedContractCall(t, issue, definition(1), token).Want(200)
	stored, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issue))
	if err != nil || stored.MergeAllowed || stored.MergeGrantedBy.Valid {
		t.Fatal("authorization notes changed the structured merge grant")
	}
	other, _, otherOwner := externalAgentFixture(t, "Contract Other", "contract-other")
	otherToken := externalTokenFixture(t, other, otherOwner, time.Now().Add(time.Hour))
	dbfx.Exec(t, "UPDATE issue SET coordinator_type='agent',coordinator_id=$2 WHERE id=$1", issue, other)
	authenticatedContractCall(t, issue, definition(2), otherToken).Want(403)
	// Even the original requester cannot edit while it is the executor.
	dbfx.Exec(t, "UPDATE issue SET assignee_type='agent',assignee_id=$2 WHERE id=$1", issue, creator)
	authenticatedContractCall(t, issue, definition(2), token).Want(403)
}

func TestContractManagedOriginalRequesterRequiresCreationScopeAndOriginator(t *testing.T) {
	issue, _ := contractFixture(t)
	runtime := dbfx.Runtime(t, "Contract creation runtime")
	creator := dbfx.Agent(t, "Contract request creator", runtime)
	parent := dbfx.Issue(t, "Creation business scope")
	other := dbfx.Issue(t, "Unrelated business scope")
	task := dbfx.Task(t, creator, testutil.Cols{"runtime_id": runtime, "issue_id": parent, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
	dbfx.Exec(t, "UPDATE issue SET creator_type='agent',creator_id=$2,origin_type='agent_create',origin_id=$3 WHERE id=$1", issue, creator, task)
	token := taskTokenFixture(t, creator, task)
	var view ContractView
	authenticatedContractCall(t, issue, definition(0), token).Want(200).JSON(&view)
	if view.Contract.Current().Author.ID != creator {
		t.Fatal("managed creation used a different actor")
	}
	continuation := dbfx.Task(t, creator, testutil.Cols{"runtime_id": runtime, "issue_id": parent, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
	authenticatedContractCall(t, issue, definition(1), taskTokenFixture(t, creator, continuation)).Want(200)
	foreignScope := dbfx.Task(t, creator, testutil.Cols{"runtime_id": runtime, "issue_id": other, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
	authenticatedContractCall(t, issue, definition(2), taskTokenFixture(t, creator, foreignScope)).Want(403)
	unattributed := dbfx.Task(t, creator, testutil.Cols{"runtime_id": runtime, "issue_id": parent, "status": "running"})
	authenticatedContractCall(t, issue, definition(2), taskTokenFixture(t, creator, unattributed)).Want(403)
	dbfx.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1", continuation)
	authenticatedContractCall(t, issue, definition(2), taskTokenFixture(t, creator, continuation)).Want(403)
	// The versioned executor cannot gain editing rights by reassigning itself.
	self, selfExecutor := contractFixture(t)
	dbfx.Exec(t, "UPDATE issue SET creator_type='agent',creator_id=$2,origin_type='agent_create',origin_id=$3 WHERE id=$1", self, selfExecutor, task)
	contractCall(t, self, definition(0), "").Want(200)
	dbfx.Exec(t, "UPDATE issue SET assignee_id=$2 WHERE id=$1", self, creator)
	executorTask := dbfx.Task(t, selfExecutor, testutil.Cols{"runtime_id": runtime, "issue_id": parent, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
	authenticatedContractCall(t, self, definition(1), taskTokenFixture(t, selfExecutor, executorTask)).Want(403)
}
