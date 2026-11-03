package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestResultTakeoverSelectionRejectsAddressAndGenerationDrift(t *testing.T) {
	for _, change := range []string{"session", "workdir", "generation"} {
		t.Run(change, func(t *testing.T) {
			source, result, address := resultFixture(t, "selection-drift-"+change)
			finishResultFixture(t, result)
			token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
			f := takeoverFixture{result, address, testUserID, token}
			target := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "selected-A", "work_dir": "/work/selected-A", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
			options := decodeTakeoverOptions(t, callTakeoverOptions(t, f))
			if len(options.Targets) != 1 || options.Targets[0].SessionRef != takeoverSessionRef("codex", "selected-A") {
				t.Fatal("did not select A")
			}
			body := takeoverBody(t, f, delegation.KindManagedTask, target)
			body.Target.SelectionVersion = options.Targets[0].SelectionVersion
			missing := body
			missing.Target.SelectionVersion = ""
			if got := callTakeover(t, f, missing); got.Code != 400 {
				t.Fatalf("new selection without proof: %d", got.Code)
			}
			generation := queryString(t, `SELECT session_pin_generation::text FROM agent_task_queue WHERE id=$1`, target)
			sid, dir := "selected-A", "/work/selected-A"
			switch change {
			case "session":
				sid = "changed-B"
			case "workdir":
				dir = "/work/changed-B"
			case "generation":
				next := uuidToString(dbid.NewV7())
				testutil.Call(t, testHandler.PinTaskSession, withURLParam(newOwnerDaemonRequest("POST", "/session", map[string]string{"generation": generation, "next_generation": next}), "taskId", target)).Want(200)
				generation = next
			}
			pin := withURLParam(newOwnerDaemonRequest("POST", "/session", map[string]string{"session_id": sid, "work_dir": dir, "generation": generation}), "taskId", target)
			testutil.Call(t, testHandler.PinTaskSession, pin).Want(204)
			got := callTakeover(t, f, body)
			t.Logf("selected reference=%s; actual %s change; POST status=%d", options.Targets[0].SessionRef, change, got.Code)
			if got.Code != 409 {
				t.Fatalf("changed selection accepted: %d %s", got.Code, got.Body.String())
			}
			if countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID) != 0 || countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, result.IssueID) != 1 || queryString(t, `SELECT id::text FROM issue_delegation_return WHERE issue_id=$1 AND is_current`, result.IssueID) != uuidToString(address.ID) {
				t.Fatal("stale selection left side effects")
			}
		})
	}
}

func TestResultTakeoverSelectionReplayUsesAcceptedProof(t *testing.T) {
	source, result, address := resultFixture(t, "selection-replay")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, address, testUserID, token}
	target := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "selected-A", "work_dir": "/work/selected-A", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	body := takeoverBody(t, f, delegation.KindManagedTask, target)
	options := decodeTakeoverOptions(t, callTakeoverOptions(t, f))
	body.Target.SelectionVersion = options.Targets[0].SelectionVersion
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	accepted := decodeTakeover(t, callTakeover(t, f, body), 201)
	testutil.Call(t, testHandler.PinTaskSession, withURLParam(newOwnerDaemonRequest("POST", "/session", map[string]string{"session_id": "new-B", "work_dir": "/work/new-B", "generation": queryString(t, `SELECT session_pin_generation::text FROM agent_task_queue WHERE id=$1`, target)}), "taskId", target)).Want(204)
	replay := decodeTakeover(t, callTakeover(t, f, body), 200)
	if replay.ID != accepted.ID || replay.TargetReturnID != accepted.TargetReturnID {
		t.Fatal("replay re-resolved mutable source")
	}
	refreshed := decodeTakeoverOptions(t, callTakeoverOptions(t, f))
	changed := body
	changed.Target.SelectionVersion = refreshed.Targets[0].SelectionVersion
	if got := callTakeover(t, f, changed); got.Code != 409 {
		t.Fatal("same key accepted a changed selection proof")
	}
	changed.ClientRequestID = uuidToString(dbid.NewV7())
	changed.ExpectedCurrentReturnID = refreshed.CurrentReturnID
	next := decodeTakeover(t, callTakeover(t, f, changed), 201)
	replay = decodeTakeover(t, callTakeover(t, f, body), 200)
	if replay.TargetReturnID != accepted.TargetReturnID || replay.CurrentReturnID != next.TargetReturnID {
		t.Fatal("old proof replay moved current backwards")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, target)
	decodeTakeover(t, callTakeover(t, f, body), 200)
}

func TestResultTakeoverPiSessionPathIsNotDisplayMetadata(t *testing.T) {
	source, result, address := resultFixture(t, "pi-display")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, address, testUserID, token}
	dbfx.Exec(t, `UPDATE agent_runtime SET provider='pi' WHERE id=$1`, source.RuntimeID)
	dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "/home/private-user/.pi/private-session.jsonl", "work_dir": "/work/pi-private", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	w := callTakeoverOptions(t, f)
	out := decodeTakeoverOptions(t, w)
	if len(out.Targets) != 1 || out.Targets[0].SessionRef == "" || !strings.Contains(out.Targets[0].DisplayName, "Delegation scope") {
		t.Fatal("pi source lacks a safe readable scope label")
	}
	if strings.Contains(w.Body.String(), "/home/private-user") || strings.Contains(w.Body.String(), "/work/pi-private") {
		t.Fatalf("provider session path leaked as display data: %s", w.Body.String())
	}
}
