package handler

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func contractFixture(t *testing.T) (string, string) {
	t.Helper()
	executor := dbfx.Agent(t, "Contract executor", "")
	issue := dbfx.Issue(t, "Contract lifecycle", testutil.Cols{"assignee_type": "agent", "assignee_id": executor, "coordinator_type": "member", "coordinator_id": testUserID})
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	return issue, executor
}

func contractCall(t *testing.T, issue string, body any, agent string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/contract", body), "id", issue)
	if agent != "" {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", agent)
	}
	return testutil.Call(t, testHandler.UpdateIssueContract, req)
}

func definition(sequence int) contractRequest {
	return contractRequest{Action: "revise", ExpectedSequence: sequence, ClientRequestID: uuidToString(dbid.NewV7()), Goal: "Ship the agreement", Scope: "Issue details", Authorization: "Implement and test only; no merge", Requirements: []string{"API behavior", "Browser evidence"}}
}

func TestContractLifecyclePartialReturnAndChildClosure(t *testing.T) {
	issue, executor := contractFixture(t)
	var view ContractView
	contractCall(t, issue, definition(0), "").Want(200).JSON(&view)
	if view.Contract.Current().Number != 1 || view.PendingActor.ID != executor {
		t.Fatalf("initial contract: %+v", view)
	}
	child := dbfx.Issue(t, "Covered delivery", testutil.Cols{"parent_issue_id": issue, "status": "in_review", "coordinator_type": "member", "coordinator_id": testUserID})
	untouched := dbfx.Issue(t, "Independent follow-up", testutil.Cols{"parent_issue_id": issue, "status": "blocked"})
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "test report", ChildIDs: []string{child}}
	contractCall(t, issue, submit, executor).Want(200).JSON(&view)
	if view.CanReview || view.PendingActor.ID != testUserID {
		t.Fatalf("review responsibility: %+v", view)
	}
	// An executor cannot accept its own output, nor mutate the agreement.
	accept := contractRequest{Action: "accept", Version: 1, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0}, ChildIDs: []string{child}}
	contractCall(t, issue, accept, executor).Want(403)
	contractCall(t, issue, definition(2), executor).Want(403)
	contractCall(t, issue, accept, "").Want(200).JSON(&view)
	if len(view.Accepted) != 1 || view.PendingActor.ID != executor {
		t.Fatalf("partial acceptance: %+v", view)
	}
	var status, childStatus, untouchedStatus, storedExecutor string
	dbfx.QueryRow(t, "SELECT status, assignee_id FROM issue WHERE id=$1", issue).Scan(&status, &storedExecutor)
	dbfx.QueryRow(t, "SELECT status FROM issue WHERE id=$1", child).Scan(&childStatus)
	dbfx.QueryRow(t, "SELECT status FROM issue WHERE id=$1", untouched).Scan(&untouchedStatus)
	if status != "in_progress" || storedExecutor != executor || childStatus != "done" || untouchedStatus != "blocked" {
		t.Fatalf("incorrect closure: %s %s %s %s", status, storedExecutor, childStatus, untouchedStatus)
	}
	// Native manual completion must not bypass remaining criteria.
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, map[string]any{"status": "done"}), "id", issue)).Want(409)
	submit.ExpectedSequence, submit.Criteria, submit.ChildIDs = 3, []int{1}, nil
	submit.ClientRequestID = uuidToString(dbid.NewV7())
	contractCall(t, issue, submit, executor).Want(200)
	returned := contractRequest{Action: "return", Version: 1, ExpectedSequence: 4, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Reason: "Need refresh evidence"}
	contractCall(t, issue, returned, "").Want(200)
	submit.ExpectedSequence, submit.ClientRequestID = 5, uuidToString(dbid.NewV7())
	contractCall(t, issue, submit, executor).Want(200)
	accept.ExpectedSequence, accept.ClientRequestID, accept.DeliveryID, accept.Criteria, accept.ChildIDs = 6, uuidToString(dbid.NewV7()), submit.ClientRequestID, []int{1}, nil
	contractCall(t, issue, accept, "").Want(200).JSON(&view)
	if len(view.Accepted) != 2 || view.PendingActor != nil {
		t.Fatalf("completion: %+v", view)
	}
	dbfx.QueryRow(t, "SELECT status FROM issue WHERE id=$1", issue).Scan(&status)
	if status != "done" {
		t.Fatalf("final status=%s", status)
	}
	// Replay is idempotent, while a changed payload with the same key conflicts.
	contractCall(t, issue, accept, "").Want(200)
	accept.Reason = "different"
	contractCall(t, issue, accept, "").Want(409)
	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issue+"/contract", nil), "id", issue)
	testutil.Call(t, testHandler.GetIssueContract, req).Want(200).JSON(&view)
	if view.Contract.Sequence != 7 || len(view.Contract.Decisions) != 3 {
		t.Fatalf("persistent history: %+v", view.Contract)
	}
}

func TestContractVersionConcurrencyAndLegacyCompatibility(t *testing.T) {
	issue, executor := contractFixture(t)
	first := definition(0)
	contractCall(t, issue, first, "").Want(200)
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "version one"}
	contractCall(t, issue, submit, executor).Want(200)
	contractCall(t, issue, definition(2), "").Want(200)
	accept := contractRequest{Action: "accept", Version: 1, ExpectedSequence: 3, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0, 1}}
	contractCall(t, issue, accept, "").Want(409)
	// Distinct requests observing one sequence cannot both commit.
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		body := definition(3)
		wg.Go(func() {
			req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/contract", body), "id", issue)
			statuses <- testutil.Call(t, testHandler.UpdateIssueContract, req).Code
		})
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for s := range statuses {
		counts[s]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent writes: %v", counts)
	}
	// Native identity changes invalidate the version, including its old decisions.
	dbfx.Exec(t, "UPDATE issue SET coordinator_id=$2, coordinator_type='agent' WHERE id=$1", issue, executor)
	submit.ExpectedSequence, submit.Version, submit.ClientRequestID = 4, 3, uuidToString(dbid.NewV7())
	contractCall(t, issue, submit, executor).Want(409)
	contractCall(t, issue, definition(4), "").Want(400)
	legacy := dbfx.Issue(t, "No contract")
	var view ContractView
	testutil.Call(t, testHandler.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/api/issues/"+legacy+"/contract", nil), "id", legacy)).Want(200).JSON(&view)
	if view.Contract != nil {
		t.Fatal("legacy issue acquired a fabricated agreement")
	}
	testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+legacy, map[string]any{"status": "done"}), "id", legacy)).Want(200)
	foreignWS := dbfx.Workspace(t, "Foreign contract", fmt.Sprintf("foreign-contract-%s", legacy[:8]))
	foreign := dbfx.Issue(t, "Foreign", testutil.Cols{"workspace_id": foreignWS})
	contractCall(t, foreign, definition(0), "").Want(404)
}
