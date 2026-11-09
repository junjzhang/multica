package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestContractRevisionsAdvanceNativeRevisionAndRank(t *testing.T) {
	issue, executor := contractFixture(t)
	dbfx.Issue(t, "Destination rank", testutil.Cols{"status": "in_progress", "position": -100})
	dbfx.Exec(t, "UPDATE issue SET status='in_review',position=100 WHERE id=$1", issue)
	load := func() db.Issue {
		t.Helper()
		row, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issue))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := load()
	contractCall(t, issue, definition(0), "").Want(200)
	after := load()
	if after.Status != "in_progress" || after.Revision != before.Revision+1 || after.Position >= -100 {
		t.Fatalf("revision/rank lost on review->execution: before=%+v after=%+v", before, after)
	}
	before = after
	contractCall(t, issue, definition(1), "").Want(200)
	after = load()
	if after.Revision != before.Revision+1 || after.Position != before.Position {
		t.Fatal("same-status agreement did not advance revision or changed rank")
	}
	submit := contractRequest{Action: "submit", Version: 2, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "Ready"}
	contractCall(t, issue, submit, executor).Want(200)
	contractCall(t, issue, contractRequest{Action: "accept", Version: 2, ExpectedSequence: 3, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0, 1}}, "").Want(200)
	before = load()
	contractCall(t, issue, definition(4), "").Want(200)
	after = load()
	if before.Status != "done" || after.Status != "in_progress" || after.Revision != before.Revision+1 || after.Position >= -100 {
		t.Fatal("done->execution revision/rank lost")
	}
}

func TestContractDoneIssueCanRevokeNativeMergeAuthorization(t *testing.T) {
	issue, executor := contractFixture(t)
	update := func(body any) {
		t.Helper()
		testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue, body), "id", issue)).Want(200)
	}
	update(map[string]any{"merge_authorization": map[string]any{"allowed": true, "repository_url": "https://github.com/example/repository", "target_branch": "main"}})
	contractCall(t, issue, definition(0), "").Want(200)
	submit := contractRequest{Action: "submit", Version: 1, ExpectedSequence: 1, ClientRequestID: uuidToString(dbid.NewV7()), Criteria: []int{0, 1}, Evidence: "Done"}
	contractCall(t, issue, submit, executor).Want(200)
	contractCall(t, issue, contractRequest{Action: "accept", Version: 1, ExpectedSequence: 2, ClientRequestID: uuidToString(dbid.NewV7()), DeliveryID: submit.ClientRequestID, Criteria: []int{0, 1}}, "").Want(200)
	update(map[string]any{"merge_authorization": map[string]any{"allowed": false}})
	var view ContractView
	testutil.Call(t, testHandler.GetIssueContract, withURLParam(newRequest(http.MethodGet, "/", nil), "id", issue)).Want(200).JSON(&view)
	if !view.Stale || view.MergeAuthorization.Allowed {
		t.Fatal("revocation did not remove the real grant and invalidate the agreement")
	}
	contractCall(t, issue, definition(3), "").Want(200)
	row, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issue))
	if err != nil || row.Status != "in_progress" || row.MergeAllowed {
		t.Fatal("revised agreement did not reopen with revoked permission")
	}
}

func TestContractSnapshotChangeRequiresFreshRead(t *testing.T) {
	before := db.Issue{Title: "Same", AcceptanceCriteria: []byte(`["old"]`)}
	after := before
	after.AcceptanceCriteria = []byte(`["new"]`)
	changed := buildIssueStateSnapshot(after).changedFieldsSince(buildIssueStateSnapshot(before))
	hint := execenv.BuildIssueStateHint("issue", "in_progress", "agent", "executor", changed, true, true)
	if !strings.Contains(hint, "Read it:") || !strings.Contains(hint, "contract") || strings.Contains(hint, "That answers workflow step 1") {
		t.Fatalf("stale agreement waived the read: %s", hint)
	}
}

func TestContractClaimDetectsNoteOnlyRevision(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Contract snapshot runtime")
	agent, issue := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Contract snapshot agent")
	dbfx.Exec(t, "UPDATE issue SET assignee_type='agent',assignee_id=$2,coordinator_type='member',coordinator_id=$3 WHERE id=$1", issue, agent, testUserID)
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
	contractCall(t, issue, definition(0), "").Want(200)
	row, err := testHandler.Queries.GetIssue(ctx, parseUUID(issue))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := buildIssueStateSnapshot(row)
	snapshot.ContractIssueRevision = row.Revision
	raw, _ := json.Marshal(snapshot)
	seedPriorRunWithSnapshot(t, agent, runtimeID, issue, string(raw))
	change := definition(1)
	change.Goal = "A new agreement without editing the issue body"
	contractCall(t, issue, change, "").Want(200)
	createCommentTriggeredClaimTask(t, ctx, agent, runtimeID, issue, nil)
	response := claimCommentTask(t, runtimeID, "contract-snapshot-change")
	if !response.Task.IssueStateDeltaKnown || strings.Join(response.Task.IssueChangedFields, ",") != "contract" {
		t.Fatalf("contract change not reported: %+v", response.Task)
	}
}

func TestContractWakeupActorAndSource(t *testing.T) {
	for _, kind := range []string{"member", "external", "managed"} {
		t.Run(kind, func(t *testing.T) {
			issue, _ := contractFixture(t)
			target := dbfx.Agent(t, "Contract watcher "+uuidToString(dbid.NewV7()), testRuntimeID)
			actorType, actorID, token, source := "member", testUserID, "", ""
			if kind == "external" {
				creator, _, owner := externalAgentFixture(t, "Contract event creator", "contract-event")
				actorType, actorID = "agent", creator
				token = externalTokenFixture(t, creator, owner, time.Now().Add(time.Hour))
				dbfx.Exec(t, "UPDATE issue SET creator_type='agent',creator_id=$2 WHERE id=$1", issue, creator)
			}
			if kind == "managed" {
				creator := dbfx.Agent(t, "Contract managed event creator", testRuntimeID)
				parent := dbfx.Issue(t, "Contract creator scope")
				source = dbfx.Task(t, creator, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": parent, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID})
				actorType, actorID = "agent", creator
				token = taskTokenFixture(t, creator, source)
				dbfx.Exec(t, "UPDATE issue SET creator_type='agent',creator_id=$2,origin_type='agent_create',origin_id=$3 WHERE id=$1", issue, creator, source)
			}
			dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
			dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
			svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
			input := service.WakeupInput{AgentID: target, Kind: "event", Mode: "continuous", EventTypes: []string{"issue.updated", "issue.status_changed"}, FilterActorType: actorType, FilterActorID: actorID, Instruction: "Inspect contract"}
			watch, err := svc.Create(context.Background(), parseUUID(issue), parseUUID(testUserID), pgtype.UUID{}, input)
			if err != nil {
				t.Fatal(err)
			}
			var own pgtype.UUID
			if source != "" {
				w, err := svc.Create(context.Background(), parseUUID(issue), parseUUID(testUserID), parseUUID(source), input)
				if err != nil {
					t.Fatal(err)
				}
				own = w.ID
			}
			for sequence := range 2 {
				if token == "" {
					contractCall(t, issue, definition(sequence), "").Want(200)
				} else {
					authenticatedContractCall(t, issue, definition(sequence), token).Want(200)
				}
			}
			var gotType, gotID string
			var gotSource *string
			dbfx.QueryRow(t, "SELECT payload->>'actor_type',payload->>'actor_id',payload->>'source_task_id' FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND event_type='issue.updated'", watch.ID).Scan(&gotType, &gotID, &gotSource)
			if gotType != actorType || gotID != actorID || source == "" && gotSource != nil || source != "" && (gotSource == nil || *gotSource != source) {
				t.Fatalf("wrong wakeup actor/source: %s %s %v", gotType, gotID, gotSource)
			}
			if own.Valid && dbfx.Count(t, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1", own) != 0 {
				t.Fatal("contract write woke its own source")
			}
		})
	}
}
