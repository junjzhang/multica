package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func callTakeoverOptions(t *testing.T, f takeoverFixture) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/api/issues/%s/result-takeover-options?result_task_id=%s&from_return_id=%s", uuidToString(f.result.IssueID), uuidToString(f.result.ID), uuidToString(f.address.ID))
	return serveAuthenticated(t, f.token, http.HandlerFunc(testHandler.GetResultTakeoverOptions), "GET", path, nil, map[string]string{"id": uuidToString(f.result.IssueID)}, nil)
}

func decodeTakeoverOptions(t *testing.T, w *httptest.ResponseRecorder) ResultTakeoverOptionsResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("options HTTP%d: %s", w.Code, w.Body.String())
	}
	var out ResultTakeoverOptionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.CurrentReturnID == "" || out.Returns == nil || out.Targets == nil {
		t.Fatalf("missing authoritative projection: %s", w.Body.String())
	}
	return out
}

func TestResultTakeoverOptionsManagedScopeAndCurrentPin(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	source, result, address := resultFixture(t, "takeover-options")
	finishResultFixture(t, result)
	token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
	f := takeoverFixture{result, address, testUserID, token}
	stranger := dbfx.User(t, "private source owner", fmt.Sprintf("private-target-%d@example.com", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, stranger, "member")
	privateChat := dbfx.ChatSession(t, uuidToString(source.AgentID), testutil.Cols{"creator_id": stranger, "title": "PRIVATE CHAT TITLE MUST NOT LEAK"})
	candidates := map[string]string{}
	for _, name := range []string{"eligible", "private", "legacy", "unpinned", "closed"} {
		cols := testutil.Cols{"issue_id": source.IssueID, "runtime_id": source.RuntimeID, "status": "running", "session_id": "candidate-" + name, "work_dir": "/sensitive-workdir/" + name, "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")}
		switch name {
		case "private":
			cols["issue_id"] = nil
			cols["chat_session_id"] = privateChat
		case "legacy":
			cols["resource_control_required"] = false
		case "unpinned":
			cols["session_pin_confirmed"] = false
		case "closed":
			cols["status"] = "completed"
		}
		candidates[name] = dbfx.Task(t, uuidToString(source.AgentID), cols)
	}
	actions := countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	tasks := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1`, source.AgentID)
	w := callTakeoverOptions(t, f)
	out := decodeTakeoverOptions(t, w)
	if len(out.Targets) != 1 || out.Targets[0].SourceTaskID != candidates["eligible"] || !out.Targets[0].Available {
		t.Fatalf("history/private/unproved candidate advertised: %+v", out.Targets)
	}
	if strings.Contains(w.Body.String(), "PRIVATE CHAT") || strings.Contains(w.Body.String(), "sensitive-workdir") || strings.Contains(w.Body.String(), "candidate-private") {
		t.Fatalf("private target data leaked: %s", w.Body.String())
	}
	if countValue(t, `SELECT count(*) FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID) != actions || countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1`, source.AgentID) != tasks || out.CurrentReturnID != uuidToString(address.ID) {
		t.Fatal("GET mutated dispatch state")
	}
	if len(out.Returns) != 1 || out.Returns[0].Target == nil || out.Returns[0].Target.SessionRef != takeoverSessionRef(address.Provider.String, address.SessionID.String) {
		t.Fatal("historical target did not show its frozen session")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=NULL,chat_session_id=$2 WHERE id=$1`, source.ID, privateChat)
	hidden := decodeTakeoverOptions(t, callTakeoverOptions(t, f))
	if hidden.Returns[0].Target != nil || len(hidden.Targets) != 1 {
		t.Fatal("agent management leaked historical private-chat target details")
	}
	// Losing original provenance hides details but does not prevent selecting a
	// different, currently proved source of the same dispatcher.
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id=$1`, source.ID)
	out = decodeTakeoverOptions(t, callTakeoverOptions(t, f))
	if out.Returns[0].Target != nil || out.CurrentReturnID != uuidToString(address.ID) || len(out.Targets) != 1 {
		t.Fatalf("lost original source became missing binding/candidates: %+v", out)
	}
	dbfx.Cleanup(t, `DELETE FROM delegation_result_takeover WHERE result_task_id=$1`, result.ID)
	action := decodeTakeover(t, callTakeover(t, f, takeoverBody(t, f, delegation.KindManagedTask, candidates["eligible"])), 201)
	if action.TargetReturnID == uuidToString(address.ID) {
		t.Fatal("selected target overwrote lost original")
	}
}

func TestResultTakeoverOptionsExternalOwnershipAndSafeDisplay(t *testing.T) {
	f := externalTakeoverFixture(t)
	b := takeoverExternalSession(t, f, "options-B")
	out := decodeTakeoverOptions(t, callTakeoverOptions(t, f))
	if len(out.Targets) != 2 || len(out.Returns) != 1 || out.Returns[0].Target == nil {
		t.Fatalf("registered targets unavailable: %+v", out)
	}
	found := false
	for _, option := range out.Targets {
		if option.ExternalSessionID == b {
			found = option.Kind == delegation.KindExternalSession && option.SessionRef == takeoverSessionRef("paseo", "session-options-B") && option.Available
		}
	}
	if !found {
		t.Fatal("explicit registered selector missing")
	}
	stranger := dbfx.User(t, "options stranger", fmt.Sprintf("options-%d@example.com", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, stranger, "member")
	path := fmt.Sprintf("/options?result_task_id=%s&from_return_id=%s", uuidToString(f.result.ID), uuidToString(f.address.ID))
	testutil.Call(t, testHandler.GetResultTakeoverOptions, withURLParam(newRequestAs(stranger, "GET", path, nil), "id", uuidToString(f.result.IssueID))).Want(403)
	machine := f
	machine.token = externalTokenFixture(t, uuidToString(f.address.DispatcherAgentID), f.owner, time.Now().Add(time.Hour))
	if w := callTakeoverOptions(t, machine); w.Code != 403 {
		t.Fatalf("machine read allowed: %d", w.Code)
	}
	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, f.owner)
	out = decodeTakeoverOptions(t, callTakeoverOptions(t, f))
	if len(out.Targets) != 0 || out.Returns[0].Target != nil {
		t.Fatal("revoked owner still advertised as available")
	}
}

func TestResultTakeoverOptionsReadFailureIsNotEmptyCandidates(t *testing.T) {
	f := externalTakeoverFixture(t)
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `LOCK TABLE external_agent_session IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/options?result_task_id=%s&from_return_id=%s", uuidToString(f.result.ID), uuidToString(f.address.ID))
	request := withURLParam(newRequest("GET", path, nil), "id", uuidToString(f.result.IssueID))
	ctx, cancel := context.WithTimeout(request.Context(), 150*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	testHandler.GetResultTakeoverOptions(w, request.WithContext(ctx))
	if w.Code != 500 || strings.Contains(w.Body.String(), `"targets"`) {
		t.Fatalf("storage failure became empty candidates: HTTP%d %s", w.Code, w.Body.String())
	}
}
