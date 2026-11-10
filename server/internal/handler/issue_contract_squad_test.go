package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func squadContractFixture(t *testing.T, external bool) (issue, squad, leader, token string) {
	t.Helper()
	if external {
		var owner string
		leader, _, owner = externalAgentFixture(t, "Contract squad leader", "contract-squad")
		token = externalTokenFixture(t, leader, owner, time.Now().Add(time.Hour))
	} else {
		leader = dbfx.Agent(t, "Contract squad leader", testRuntimeID)
	}
	squad = dbfx.Squad(t, "Contract squad", leader)
	dbfx.SquadMember(t, squad, "agent", leader, testutil.Cols{"role": "leader"})
	issue = dbfx.Issue(t, "Squad agreement", testutil.Cols{"assignee_type": "squad", "assignee_id": squad, "coordinator_type": "member", "coordinator_id": testUserID})
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	if !external {
		task := dbfx.Task(t, leader, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issue, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
		token = taskTokenFixture(t, leader, task)
	}
	return
}

func TestContractSquadAuthenticatedDelivery(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "managed"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			issue, squad, leader, token := squadContractFixture(t, external)
			var view ContractView
			contractCall(t, issue, definition(0), "").Want(200).JSON(&view)
			if view.Stale || view.Executor.Type != "squad" || view.Executor.ID != squad || view.ExecutorRepresentative.ID != leader {
				t.Fatalf("squad responsibility or representative: %+v", view)
			}
			submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "Squad evidence"}
			worker := dbfx.Agent(t, "Ordinary squad worker", testRuntimeID)
			dbfx.SquadMember(t, squad, "agent", worker)
			workerTask := dbfx.Task(t, worker, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issue, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
			authenticatedContractCall(t, issue, submit, taskTokenFixture(t, worker, workerTask)).Want(403)
			authenticatedContractCall(t, issue, definition(1), token).Want(403)
			authenticatedContractCall(t, issue, submit, token).Want(200).JSON(&view)
			if view.Contract.Pending().Author != (issuecontract.Actor{Type: "agent", ID: leader}) || view.Executor.ID != squad || view.CanReview {
				t.Fatalf("delivery must retain the real author and squad executor: %+v", view)
			}
			accept := contractRequest{Action: "accept", Version: 1, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0}}
			authenticatedContractCall(t, issue, accept, token).Want(403)
			contractCall(t, issue, accept, "").Want(200).JSON(&view)
			if len(view.Accepted) != 1 || view.PendingActor.ID != squad {
				t.Fatalf("partial acceptance lost squad responsibility: %+v", view)
			}
			submit.ExpectedSequence, submit.ClientRequestID, submit.Criteria = 3, uuidToString(dbid.NewV7()), []int{1}
			authenticatedContractCall(t, issue, submit, token).Want(200)
			returned := contractRequest{Action: "return", Version: 1, ExpectedSequence: 4, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Reason: "Refresh evidence"}
			contractCall(t, issue, returned, "").Want(200)
			submit.ExpectedSequence, submit.ClientRequestID = 5, uuidToString(dbid.NewV7())
			authenticatedContractCall(t, issue, submit, token).Want(200)
			accept.ExpectedSequence, accept.ClientRequestID, accept.DeliveryID, accept.Criteria = 6, uuidToString(dbid.NewV7()), submit.ClientRequestID, []int{1}
			contractCall(t, issue, accept, "").Want(200).JSON(&view)
			var status, assigneeType, assigneeID string
			dbfx.QueryRow(t, "SELECT status,assignee_type,assignee_id FROM issue WHERE id=$1", issue).Scan(&status, &assigneeType, &assigneeID)
			if status != "done" || assigneeType != "squad" || assigneeID != squad || len(view.Accepted) != 2 {
				t.Fatalf("final acceptance: %s %s %s %+v", status, assigneeType, assigneeID, view)
			}
		})
	}
}

func TestContractSquadLeaderChangeInvalidatesPendingAndAuthority(t *testing.T) {
	issue, squad, leader, token := squadContractFixture(t, true)
	dbfx.Exec(t, "UPDATE issue SET creator_type='agent',creator_id=$2 WHERE id=$1", issue, leader)
	authenticatedContractCall(t, issue, definition(0), token).Want(403)
	contractCall(t, issue, definition(0), "").Want(200)
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "Original leader evidence"}
	authenticatedContractCall(t, issue, submit, token).Want(200)
	newLeader := dbfx.Agent(t, "Replacement leader", testRuntimeID)
	testutil.Call(t, testHandler.UpdateSquad, squadScopeReq(testUserID, http.MethodPatch, "/api/squads", map[string]any{"leader_id": newLeader}, map[string]string{"id": squad})).Want(200)
	var view ContractView
	testutil.Call(t, testHandler.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/", nil), "id", issue)).Want(200).JSON(&view)
	if !view.Stale || view.CanSubmit || view.CanReview || view.ExecutorRepresentative.ID != newLeader || view.Contract.Pending().Author.ID != leader {
		t.Fatalf("rotation lost history or retained authority: %+v", view)
	}
	accept := contractRequest{Action: "accept", Version: 1, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0, 1}}
	contractCall(t, issue, accept, "").Want(409)
	// The former representative remains excluded from maintaining its own agreement.
	authenticatedContractCall(t, issue, definition(2), token).Want(403)
	contractCall(t, issue, definition(2), "").Want(200)
	submit.Version, submit.ExpectedSequence, submit.ClientRequestID = 2, 3, uuidToString(dbid.NewV7())
	authenticatedContractCall(t, issue, submit, token).Want(403)
	contractCall(t, issue, submit, newLeader).Want(200).JSON(&view)
	if len(view.Contract.Deliveries) != 2 || view.Contract.Deliveries[0].Author.ID != leader || view.Contract.Deliveries[0].Evidence != "Original leader evidence" || view.Contract.Deliveries[1].Author.ID != newLeader {
		t.Fatalf("rotation rewrote delivery authors or evidence: %+v", view.Contract)
	}
}

func TestContractSquadRejectsInactiveRepresentativeAndSelfAcceptance(t *testing.T) {
	for _, invalidation := range []string{"archive squad", "archive leader", "remove leader membership", "leader is acceptor"} {
		t.Run(invalidation, func(t *testing.T) {
			issue, squad, leader, token := squadContractFixture(t, true)
			contractCall(t, issue, definition(0), "").Want(200)
			switch invalidation {
			case "archive squad":
				dbfx.Exec(t, "UPDATE squad SET archived_at=now() WHERE id=$1", squad)
			case "archive leader":
				dbfx.Exec(t, "UPDATE agent SET archived_at=now() WHERE id=$1", leader)
			case "remove leader membership":
				dbfx.Exec(t, "DELETE FROM squad_member WHERE squad_id=$1 AND member_id=$2", squad, leader)
			case "leader is acceptor":
				dbfx.Exec(t, "UPDATE issue SET coordinator_type='agent',coordinator_id=$2 WHERE id=$1", issue, leader)
			}
			submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "Must be rejected"}
			status := 409
			if invalidation == "archive leader" {
				status = 401 // Auth rejects archived external identities first.
			}
			authenticatedContractCall(t, issue, submit, token).Want(status)
			contractCall(t, issue, definition(1), "").Want(400)
			if dbfx.Count(t, "SELECT count(*) FROM issue_contract WHERE issue_id=$1 AND jsonb_array_length(data->'deliveries')<>0", issue) != 0 {
				t.Fatal("invalid representative appended delivery")
			}
		})
	}
}

func TestContractSquadSubmitConflictsWithLeaderChange(t *testing.T) {
	issue, squad, _, token := squadContractFixture(t, true)
	contractCall(t, issue, definition(0), "").Want(200)
	newLeader := dbfx.Agent(t, "Concurrent replacement", testRuntimeID)
	dbfx.SquadMember(t, squad, "agent", newLeader)
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "UPDATE squad SET leader_id=$2 WHERE id=$1", squad, newLeader); err != nil {
		t.Fatal(err)
	}
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Concurrent evidence"}
	authenticatedContractCall(t, issue, submit, token).Want(409)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	authenticatedContractCall(t, issue, submit, token).Want(409)
}

func TestContractSquadManagedRevisionDoesNotInvertRuntimeTeardownLocks(t *testing.T) {
	runtime := dbfx.Runtime(t, "Squad contract teardown runtime")
	leader := dbfx.Agent(t, "Teardown squad leader", runtime)
	requester := dbfx.Agent(t, "Squad original requester", runtime)
	squad := dbfx.Squad(t, "Teardown squad", leader)
	dbfx.SquadMember(t, squad, "agent", leader)
	parent := dbfx.Issue(t, "Original request scope")
	task := dbfx.Task(t, requester, testutil.Cols{"runtime_id": runtime, "issue_id": parent, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
	issue := dbfx.Issue(t, "Managed squad agreement", testutil.Cols{"assignee_type": "squad", "assignee_id": squad, "coordinator_type": "member", "coordinator_id": testUserID, "creator_type": "agent", "creator_id": requester, "origin_type": "agent_create", "origin_id": task})
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	token := taskTokenFixture(t, requester, task)
	ctx := t.Context()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Runtime teardown locks Agents before cancelling their tasks. A revision
	// already holds its source task, so it must not wait on this Agent lock.
	if _, err := tx.Exec(ctx, "SELECT id FROM agent WHERE id=$1 FOR UPDATE", leader); err != nil {
		t.Fatal(err)
	}
	result := make(chan int, 1)
	go func() {
		result <- authenticatedContractCall(t, issue, definition(0), token).Code
	}()
	select {
	case status := <-result:
		if status != 409 {
			t.Fatalf("managed revision: %d", status)
		}
	case <-time.After(2 * time.Second):
		_ = tx.Rollback(ctx)
		<-result
		t.Fatal("revision waited on the Agent lock after locking its source task")
	}
	if _, err := tx.Exec(ctx, "UPDATE agent_task_queue SET status='cancelled',completed_at=now() WHERE id=$1", task); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestContractSquadSubmitConflictsWithConcurrentArchival(t *testing.T) {
	for _, target := range []string{"squad", "agent"} {
		t.Run(target, func(t *testing.T) {
			issue, squad, leader, token := squadContractFixture(t, true)
			contractCall(t, issue, definition(0), "").Want(200)
			tx, err := testPool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			id := squad
			if target == "agent" {
				id = leader
			}
			if _, err := tx.Exec(t.Context(), "UPDATE "+target+" SET archived_at=now() WHERE id=$1", id); err != nil {
				t.Fatal(err)
			}
			submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "Archive must refuse delivery"}
			authenticatedContractCall(t, issue, submit, token).Want(409)
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			status := 409
			if target == "agent" {
				status = 401
			}
			authenticatedContractCall(t, issue, submit, token).Want(status)
			if dbfx.Count(t, "SELECT jsonb_array_length(data->'deliveries') FROM issue_contract WHERE issue_id=$1", issue) != 0 {
				t.Fatal("archival race appended delivery")
			}
		})
	}
}
