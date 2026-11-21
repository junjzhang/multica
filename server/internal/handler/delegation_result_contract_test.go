package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// storeResultContract writes a one-version contract with the given deliveries
// onto the delegated issue; the gate reads nothing else from it.
func storeResultContract(t *testing.T, issueID string, sequence int, deliveries ...issuecontract.Delivery) {
	t.Helper()
	state := issuecontract.Empty()
	state.Sequence = sequence
	state.Versions = append(state.Versions, issuecontract.Version{Number: 1, Goal: "Ship", Criteria: []string{"Works"}, CreatedAt: time.Now().UTC()})
	state.Deliveries = append(state.Deliveries, deliveries...)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `INSERT INTO issue_contract (issue_id, workspace_id, data) VALUES ($1, $2, $3)`, issueID, testWorkspaceID, data)
	dbfx.Cleanup(t, `DELETE FROM issue_contract WHERE issue_id=$1`, issueID)
}

func reconcileResults(t *testing.T) {
	t.Helper()
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContractedResultReturnsOnlyWhenDispatcherMustAct(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	cases := []struct {
		name     string
		prepare  func(t *testing.T, result db.AgentTaskQueue)
		finish   func(t *testing.T, result db.AgentTaskQueue)
		wantSent bool
	}{
		{name: "progress-only", prepare: func(t *testing.T, result db.AgentTaskQueue) {
			storeResultContract(t, uuidToString(result.IssueID), 1)
		}},
		{name: "earlier-delivery-by-another-run", prepare: func(t *testing.T, result db.AgentTaskQueue) {
			storeResultContract(t, uuidToString(result.IssueID), 3, issuecontract.Delivery{ID: "earlier", Version: 1, TaskID: uuidToString(dbid.NewV7())})
		}},
		{name: "submitted-by-this-run", wantSent: true, prepare: func(t *testing.T, result db.AgentTaskQueue) {
			storeResultContract(t, uuidToString(result.IssueID), 2, issuecontract.Delivery{ID: "this-run", Version: 1, TaskID: uuidToString(result.ID)})
		}},
		{name: "handed-back-as-blocked", wantSent: true, prepare: func(t *testing.T, result db.AgentTaskQueue) {
			dbfx.Exec(t, `UPDATE issue SET status='blocked' WHERE id=$1`, result.IssueID)
			storeResultContract(t, uuidToString(result.IssueID), 1)
		}},
		{name: "failed-run", wantSent: true, prepare: func(t *testing.T, result db.AgentTaskQueue) {
			storeResultContract(t, uuidToString(result.IssueID), 1)
		}, finish: func(t *testing.T, result db.AgentTaskQueue) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',started_at=now(),completed_at=now(),terminal_reported_at=now(),delegation_outcome_final=true WHERE id=$1`, result.ID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, result, _ := resultFixture(t, "contract-gate-"+tc.name)
			tc.prepare(t, result)
			if tc.finish != nil {
				tc.finish(t, result)
			} else {
				finishResultFixture(t, result)
			}
			reconcileResults(t)

			var reason, payload string
			dbfx.QueryRow(t, `SELECT COALESCE(l.notification_error,''), COALESCE(m.payload::text,'') FROM agent_task_delegation_return l LEFT JOIN agent_mailbox_message m ON m.id=l.notification_delivery_id WHERE l.task_id=$1`, result.ID).Scan(&reason, &payload)
			if !tc.wantSent {
				if reason != service.ResultSuppressedNoAction || payload != "" {
					t.Fatalf("progress-only run woke the dispatcher: reason=%q payload=%q", reason, payload)
				}
				if n := countValue(t, `SELECT count(*) FROM agent_task_delegation_return l WHERE l.task_id=$1 AND l.notification_task_id IS NOT NULL`, result.ID); n != 0 {
					t.Fatalf("suppressed return still queued %d handling tasks", n)
				}
				final, err := testHandler.Queries.GetAgentTask(ctx, result.ID)
				if err != nil {
					t.Fatal(err)
				}
				rows := []AgentTaskResponse{taskToResponse(final, testWorkspaceID)}
				if err := testHandler.hydrateResultNotifications(ctx, parseUUID(testWorkspaceID), result.IssueID, rows); err != nil {
					t.Fatal(err)
				}
				notes := *rows[0].ResultNotifications
				if len(notes) != 1 || notes[0].Status != "suppressed" || notes[0].Error != "" {
					t.Fatalf("suppressed return rendered as %+v", notes)
				}
				return
			}
			if reason != "" || payload == "" {
				t.Fatalf("dispatcher action was not returned: reason=%q payload=%q", reason, payload)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(payload), &fields); err != nil {
				t.Fatal(err)
			}
			var stored issuecontract.State
			var raw []byte
			dbfx.QueryRow(t, `SELECT data FROM issue_contract WHERE issue_id=$1`, result.IssueID).Scan(&raw)
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			if fields["result_task_id"] != uuidToString(result.ID) ||
				fields["contract_sequence"] != float64(stored.Sequence) ||
				fields["contract_delivery_count"] != float64(len(stored.Deliveries)) {
				t.Fatalf("return lacks ordering facts: %+v (contract sequence=%d deliveries=%d)", fields, stored.Sequence, len(stored.Deliveries))
			}
		})
	}
}

func TestUncontractedResultPayloadIsUnchanged(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	_, result, _ := resultFixture(t, "contract-gate-none")
	finishResultFixture(t, result)
	reconcileResults(t)
	var payload []byte
	dbfx.QueryRow(t, `SELECT m.payload FROM agent_task_delegation_return l JOIN agent_mailbox_message m ON m.id=l.notification_delivery_id WHERE l.task_id=$1`, result.ID).Scan(&payload)
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["contract_sequence"]; ok {
		t.Fatalf("uncontracted return carries contract facts: %+v", fields)
	}
	if fields["result_task_id"] != uuidToString(result.ID) || fields["outcome"] != "completed" {
		t.Fatalf("uncontracted return changed: %+v", fields)
	}
}

func TestContractSubmitRecordsOnlyTheExecutorsOwnRun(t *testing.T) {
	issue, executor := contractFixture(t)
	contractCall(t, issue, definition(0), "").Want(200)
	runtimeID := dbfx.Runtime(t, "contract submit runtime")
	own := dbfx.Task(t, executor, testutil.Cols{"issue_id": issue, "runtime_id": runtimeID, "status": "running"})
	other := dbfx.Agent(t, "Contract bystander "+uuidToString(dbid.NewV7()), "")
	foreign := dbfx.Task(t, other, testutil.Cols{"issue_id": issue, "runtime_id": runtimeID, "status": "completed"})

	submit := func(sequence int, taskID string) issuecontract.Delivery {
		t.Helper()
		body := contractRequest{Action: "submit", Version: 1, ExpectedSequence: sequence, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0}, Evidence: "report"}
		req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issue+"/contract", body), "id", issue)
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", executor)
		req.Header.Set("X-Task-ID", taskID)
		var view ContractView
		testutil.Call(t, testHandler.UpdateIssueContract, req).Want(200).JSON(&view)
		return view.Contract.Deliveries[len(view.Contract.Deliveries)-1]
	}
	first := submit(1, own)
	if first.TaskID != own {
		t.Fatalf("own run not recorded: %+v", first)
	}
	returned := contractRequest{Action: "return", Version: 1, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: first.ID, Reason: "redo"}
	contractCall(t, issue, returned, "").Want(200)
	if got := submit(3, foreign); got.TaskID != "" {
		t.Fatalf("another agent's run was credited with the submit: %+v", got)
	}
}
