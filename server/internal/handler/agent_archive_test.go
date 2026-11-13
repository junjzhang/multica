package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func archivePreflightCall(t *testing.T, agentID string) (int, agentArchivePreflightResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ArchiveAgentPreflight(w, withURLParam(
		newRequest(http.MethodGet, "/api/agents/"+agentID+"/archive-preflight", nil), "id", agentID))
	var resp agentArchivePreflightResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode preflight: %v", err)
		}
	}
	return w.Code, resp, w.Body.String()
}

func blockersByCode(blockers []ArchiveBlocker) map[string]ArchiveBlocker {
	out := map[string]ArchiveBlocker{}
	for _, blocker := range blockers {
		out[blocker.Code] = blocker
	}
	return out
}

func insertArchiveContract(t *testing.T, issue, executor, acceptor string) {
	t.Helper()
	insertArchiveContractWith(t, issue, executor, acceptor, map[string]any{})
}

func insertArchiveContractWith(t *testing.T, issue, executor, acceptor string, basis map[string]any) {
	t.Helper()
	state := map[string]any{
		"sequence": 1,
		"versions": []map[string]any{{
			"number":             1,
			"goal":               "archive preflight",
			"scope":              "test",
			"authorization_note": "test",
			"criteria":           []string{"criterion"},
			"executor":           map[string]any{"type": "agent", "id": executor},
			"acceptor":           map[string]any{"type": "agent", "id": acceptor},
			"basis":              basis,
			"author":             map[string]any{"type": "member", "id": testUserID},
			"created_at":         "2026-01-01T00:00:00Z",
		}},
		"deliveries": []any{},
		"decisions":  []any{},
		"requests":   []any{},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, "INSERT INTO issue_contract (issue_id, workspace_id, data) VALUES ($1, $2, $3)", issue, testWorkspaceID, string(raw))
	dbfx.Cleanup(t, "DELETE FROM issue_contract WHERE issue_id=$1", issue)
}

func archiveMessage(t *testing.T, agentID, sessionID, status string) string {
	t.Helper()
	return dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
		"workspace_id":       testWorkspaceID,
		"recipient_agent_id": agentID,
		"recipient_type":     "agent",
		"target_session_id":  sessionID,
		"source_type":        "ci_run",
		"source_key":         "archive:" + uuidToString(dbid.NewV7()),
		"kind":               "ci_terminal",
		"payload":            testutil.Raw("'{\"run_id\":\"archive\"}'::jsonb"),
		"delivery_status":    status,
	})
}

// The preview must name every reference category the design lists, mark the
// blocking ones, and stay read-only.
func TestExternalAgentArchivePreflightReportsEveryReference(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Archive preflight scan", "preflight-daemon")
	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK {
		t.Fatalf("preflight: %d %s", code, body)
	}
	if !resp.Archivable || len(resp.Blockers) != 0 {
		t.Fatalf("fresh agent preflight: %+v", resp)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent WHERE id=$1 AND archived_at IS NOT NULL", agent); n != 0 {
		t.Fatal("preflight archived the agent")
	}

	// A pending notification is reported but withdrawn by the archive itself.
	if pending := archiveMessage(t, agent, session, "pending"); pending == "" {
		t.Fatal("pending message insert failed")
	}
	code, resp, body = archivePreflightCall(t, agent)
	if code != http.StatusOK {
		t.Fatalf("preflight with pending: %d %s", code, body)
	}
	blockers := blockersByCode(resp.Blockers)
	if !resp.Archivable || blockers["pending_messages"].Blocking || blockers["pending_messages"].Count != 1 {
		t.Fatalf("pending notification should be reported as non-blocking: %+v", resp)
	}

	// Every remaining category becomes a blocking reference.
	executor := dbfx.Agent(t, "Archive contract executor", testRuntimeID)
	dbfx.Squad(t, "Archive default leader squad", agent)
	assigned := dbfx.Issue(t, "Archive assigned issue", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})
	dbfx.Issue(t, "Archive leading issue", testutil.Cols{"leader_agent_id": agent})
	dbfx.Issue(t, "Archive coordinated issue", testutil.Cols{"coordinator_type": "agent", "coordinator_id": agent})
	issue := dbfx.Issue(t, "Archive contract issue")
	insertArchiveContract(t, issue, agent, executor)
	dbfx.Task(t, agent, testutil.Cols{"status": "queued", "runtime_id": testRuntimeID, "context": testutil.Raw("'{\"background_wait\":{}}'::jsonb")})
	archiveMessage(t, agent, session, "leased")
	dbfx.Insert(t, "issue_delegation_return", testutil.Cols{
		"workspace_id":        testWorkspaceID,
		"issue_id":            assigned,
		"dispatcher_agent_id": agent,
		"return_kind":         "external_session",
		"external_session_id": session,
		"is_current":          true,
	})

	code, resp, body = archivePreflightCall(t, agent)
	if code != http.StatusOK {
		t.Fatalf("blocked preflight: %d %s", code, body)
	}
	if resp.Archivable {
		t.Fatalf("blocked agent reported archivable: %+v", resp)
	}
	blockers = blockersByCode(resp.Blockers)
	for _, want := range []string{
		"in_flight_tasks", "active_waits", "unaccepted_contracts", "unverified_messages",
		"current_returns", "assigned_issues", "task_leaderships", "coordinated_issues",
		"squad_default_leader", "squad_memberships", "pending_messages",
	} {
		blocker, ok := blockers[want]
		if !ok || blocker.Count == 0 || len(blocker.SampleIDs) == 0 {
			t.Errorf("missing %s blocker: %+v", want, resp.Blockers)
			continue
		}
		if want != "pending_messages" && !blocker.Blocking {
			t.Errorf("%s should block the archive", want)
		}
	}
}

func TestExternalAgentArchiveRefusesInFlightReferencesAndKeepsCredential(t *testing.T) {
	agent, session, owner := externalAgentFixture(t, "Archive refusal", "refusal-daemon")
	token := externalSessionTokenFixture(t, agent, session, owner, time.Now().Add(time.Hour))
	dbfx.Issue(t, "Archive refusal issue", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})

	rec := testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Want(http.StatusConflict)
	var refusal struct {
		Code     string           `json:"code"`
		Blockers []ArchiveBlocker `json:"blockers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode refusal: %v: %s", err, rec.Body.String())
	}
	if refusal.Code != "agent_has_in_flight_references" || len(blockersByCode(refusal.Blockers)) == 0 {
		t.Fatalf("refusal missing blockers: %s", rec.Body.String())
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent WHERE id=$1 AND archived_at IS NULL", agent); n != 1 {
		t.Fatal("refused archive archived the agent")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM external_agent_token WHERE agent_id=$1 AND revoked_at IS NULL", agent); n != 1 {
		t.Fatal("refused archive revoked a live credential")
	}

	// Resolving the reference makes the same call succeed and revoke the
	// credential in the same transaction.
	pending := archiveMessage(t, agent, session, "pending")
	dbfx.Exec(t, "UPDATE issue SET status='done' WHERE assignee_type='agent' AND assignee_id=$1", agent)
	testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Want(http.StatusOK)
	if n := dbfx.Count(t, "SELECT count(*) FROM agent WHERE id=$1 AND archived_at IS NOT NULL", agent); n != 1 {
		t.Fatal("archive did not write archived_at")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM external_agent_token WHERE agent_id=$1 AND revoked_at IS NULL", agent); n != 0 {
		t.Fatal("archive left a live credential")
	}
	var withdrawn bool
	dbfx.QueryRow(t, "SELECT withdrawn_at IS NOT NULL FROM agent_mailbox_message WHERE id=$1", pending).Scan(&withdrawn)
	if !withdrawn {
		t.Fatal("archive did not withdraw the pending notification")
	}
	if token == "" {
		t.Fatal("fixture token missing")
	}
}

// The preview is advisory: the reference set is re-checked under the archive
// lock, so a reference that lands between the two still blocks.
func TestExternalAgentArchiveRechecksReferencesAfterPreview(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive recheck", "recheck-daemon")
	if code, resp, body := archivePreflightCall(t, agent); code != http.StatusOK || !resp.Archivable {
		t.Fatalf("clean preflight: %d %+v %s", code, resp, body)
	}
	dbfx.Issue(t, "Late archive reference", testutil.Cols{"assignee_type": "agent", "assignee_id": agent})
	testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Want(http.StatusConflict)
	if n := dbfx.Count(t, "SELECT count(*) FROM agent WHERE id=$1 AND archived_at IS NULL", agent); n != 1 {
		t.Fatal("archive ignored a reference created after the preview")
	}
}

func TestExternalAgentArchiveSquadReferences(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive squad references", "archive-squad-daemon")
	squad := dbfx.Squad(t, "Archive squad references", agent)
	dbfx.Exec(t, "UPDATE squad SET archived_at=now() WHERE id=$1", squad)

	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK || !resp.Archivable || len(resp.Blockers) != 0 {
		t.Fatalf("archived squad blocked preflight: %d %+v %s", code, resp, body)
	}

	// The final scan must see a squad restored after the preview.
	dbfx.Exec(t, "UPDATE squad SET archived_at=NULL WHERE id=$1", squad)
	rec := testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Want(http.StatusConflict)
	var refusal struct {
		Blockers []ArchiveBlocker `json:"blockers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	blockers := blockersByCode(refusal.Blockers)
	for _, key := range []string{"squad_default_leader", "squad_memberships"} {
		if blockers[key].Count != 1 || !blockers[key].Blocking {
			t.Fatalf("restored squad missing %s: %+v", key, refusal.Blockers)
		}
	}
	code, resp, body = archivePreflightCall(t, agent)
	if code != http.StatusOK || resp.Archivable || len(blockersByCode(resp.Blockers)) != 2 {
		t.Fatalf("active squad preflight: %d %+v %s", code, resp, body)
	}

	dbfx.Exec(t, "UPDATE squad SET archived_at=now() WHERE id=$1", squad)
	testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Want(http.StatusOK)
	var got SquadResponse
	testutil.Call(t, testHandler.GetSquad, squadScopeReq("", http.MethodGet, "/api/squads/"+squad, nil, map[string]string{"id": squad})).Want(http.StatusOK).JSON(&got)
	if got.LeaderID != agent {
		t.Fatalf("archived squad leader = %s, want %s", got.LeaderID, agent)
	}
}

// A new dispatch and an archive race on the same agent row. Exactly one wins,
// and the archive can never commit while leaving the new assignment behind.
func TestExternalAgentArchiveSerializesWithNewAssignment(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Archive race", "race-daemon")
	issue := dbfx.Issue(t, "Race archive with assignment")
	assignment := map[string]any{"assignee_type": "agent", "assignee_id": agent, "recipient_session_id": session}

	type result struct {
		op   string
		code int
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan result, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		results <- result{"assign", testutil.Call(t, testHandler.UpdateIssue, withURLParam(
			newRequest(http.MethodPut, "/api/issues/"+issue, assignment), "id", issue)).Code}
	}()
	go func() {
		defer wg.Done()
		<-start
		results <- result{"archive", testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
			newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Code}
	}()
	close(start)
	wg.Wait()
	close(results)
	codes := map[string]int{}
	for res := range results {
		codes[res.op] = res.code
	}
	if len(codes) != 2 {
		t.Fatalf("race produced %v", codes)
	}
	archived := dbfx.Count(t, "SELECT count(*) FROM agent WHERE id=$1 AND archived_at IS NOT NULL", agent) == 1
	if archived {
		if codes["assign"] == http.StatusOK {
			t.Fatalf("archive and assignment both succeeded: %v", codes)
		}
		if n := dbfx.Count(t, "SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id=$1 AND withdrawn_at IS NULL AND delivery_status IN ('pending','leased','unknown')", agent); n != 0 {
			t.Fatalf("archive committed with %d live messages", n)
		}
		if n := dbfx.Count(t, "SELECT count(*) FROM issue WHERE id=$1 AND assignee_id=$2", issue, agent); n != 0 {
			t.Fatal("archive committed while the issue was assigned to it")
		}
		return
	}
	// The assignment won: the archive must have been refused by the reference
	// it now holds, not silently committed.
	if codes["archive"] != http.StatusConflict || codes["assign"] != http.StatusOK {
		t.Fatalf("losing archive did not report a conflict: %v", codes)
	}
}

// A contract may reference the agent only as the squad execution
// representative. Basis is the whole contract document, so the representative
// must be decoded from its own field, not from an actor map.
func TestExternalAgentArchiveSeesContractRepresentative(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive representative", "representative-daemon")
	other := dbfx.Agent(t, "Representative contract executor", testRuntimeID)
	squad := dbfx.Squad(t, "Representative contract squad", other)
	issue := dbfx.Issue(t, "Archive representative contract")
	insertArchiveContractWith(t, issue, other, other, map[string]any{
		"executor":                map[string]any{"type": "squad", "id": squad},
		"criteria":                []string{"criterion"},
		"executor_representative": map[string]any{"type": "agent", "id": agent},
	})

	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK {
		t.Fatalf("preflight: %d %s", code, body)
	}
	blocker, ok := blockersByCode(resp.Blockers)["unaccepted_contracts"]
	if resp.Archivable || !ok || blocker.Count != 1 || !blocker.Blocking {
		t.Fatalf("representative-only contract not reported: %+v", resp.Blockers)
	}
}

// A current return relationship is only an obligation while its issue is open
// or a result it produced is still unhandled. Nothing clears is_current for a
// closed issue, so the count alone would block every dispatcher forever.
func TestExternalAgentArchiveReturnObligationNarrowsAfterClose(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Archive return", "return-daemon")
	issue := dbfx.Issue(t, "Closed return issue")
	dbfx.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", issue)
	returnID := dbfx.Insert(t, "issue_delegation_return", testutil.Cols{
		"workspace_id":        testWorkspaceID,
		"issue_id":            issue,
		"dispatcher_agent_id": agent,
		"return_kind":         "external_session",
		"external_session_id": session,
		"is_current":          true,
	})

	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK || !resp.Archivable {
		t.Fatalf("closed issue without results still blocks: %d %+v %s", code, resp, body)
	}

	result := dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
		"workspace_id":       testWorkspaceID,
		"recipient_agent_id": agent,
		"recipient_type":     "agent",
		"target_session_id":  session,
		"source_type":        "delegation_result",
		"source_key":         "result:" + returnID,
		"kind":               "delegation_result",
		"payload":            testutil.Raw("'{\"delegation_return_id\":\"" + returnID + "\"}'::jsonb"),
		"delivery_status":    "delivered",
		"processing_status":  "unread",
	})
	code, resp, body = archivePreflightCall(t, agent)
	if code != http.StatusOK {
		t.Fatalf("preflight with unhandled result: %d %s", code, body)
	}
	blocker, ok := blockersByCode(resp.Blockers)["current_returns"]
	if resp.Archivable || !ok || blocker.Count != 1 || !blocker.Blocking {
		t.Fatalf("unhandled result should block the return: %+v", resp.Blockers)
	}

	dbfx.Exec(t, "UPDATE agent_mailbox_message SET processing_status='handled' WHERE id=$1", result)
	code, resp, body = archivePreflightCall(t, agent)
	if code != http.StatusOK || !resp.Archivable {
		t.Fatalf("handled result should leave no obligation: %d %+v %s", code, resp, body)
	}
}

// A workspace custom status whose category is done closes the issue the same
// way the built-in done does. The return obligation must narrow for it too, or
// every dispatcher whose tasks finish on that status stays unarchivable.
func TestExternalAgentArchiveReturnObligationHonorsCustomDoneStatus(t *testing.T) {
	agent, session, _ := externalAgentFixture(t, "Archive custom done", "return-custom-daemon")
	statusKey := "shipped_" + uuidToString(dbid.NewV7())[:8]
	statusID := dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"key":          statusKey,
		"name":         "Shipped",
		"category":     "done",
		"color":        "#123456",
	})
	dbfx.Cleanup(t, "DELETE FROM issue_status WHERE id=$1", statusID)
	issue := dbfx.Issue(t, "Custom closed return issue")
	dbfx.Exec(t, "UPDATE issue SET status=$2 WHERE id=$1", issue, statusKey)
	dbfx.Insert(t, "issue_delegation_return", testutil.Cols{
		"workspace_id":        testWorkspaceID,
		"issue_id":            issue,
		"dispatcher_agent_id": agent,
		"return_kind":         "external_session",
		"external_session_id": session,
		"is_current":          true,
	})

	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK || !resp.Archivable {
		t.Fatalf("custom done status still blocks the return: %d %+v %s", code, resp, body)
	}
}

// Assigned, leading and coordinated references close on a workspace custom
// done status the same way they close on the built-in done. Otherwise an agent
// whose issues all shipped under that status could never be archived.
func TestExternalAgentArchiveReferenceBlockersHonorCustomDoneStatus(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive custom done refs", "return-custom-refs-daemon")
	statusKey := "shipped_refs_" + uuidToString(dbid.NewV7())[:8]
	statusID := dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"key":          statusKey,
		"name":         "Shipped references",
		"category":     "done",
		"color":        "#123456",
	})
	dbfx.Cleanup(t, "DELETE FROM issue_status WHERE id=$1", statusID)
	issue := dbfx.Issue(t, "Custom closed reference issue", testutil.Cols{
		"assignee_type":    "agent",
		"assignee_id":      agent,
		"leader_agent_id":  agent,
		"coordinator_type": "agent",
		"coordinator_id":   agent,
	})
	dbfx.Exec(t, "UPDATE issue SET status=$2 WHERE id=$1", issue, statusKey)

	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK || !resp.Archivable {
		t.Fatalf("custom done status still blocks issue references: %d %+v %s", code, resp, body)
	}
}

// The archive revokes credentials and is confirmed by a human owner. A machine
// credential that can manage the Agent must not archive it.
func TestExternalAgentArchiveRequiresHumanActor(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive human gate", "human-gate-daemon")
	req := newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil)
	req.Header.Set("X-Actor-Source", "external_agent")

	testutil.Call(t, testHandler.ArchiveAgent, withURLParam(req, "id", agent)).Want(http.StatusForbidden)
	if n := dbfx.Count(t, "SELECT count(*) FROM agent WHERE id=$1 AND archived_at IS NULL", agent); n != 1 {
		t.Fatal("machine credential archived the agent")
	}
}

// A pending notification is only reported as "withdrawn" when the archive
// transaction actually withdraws it. Legacy rows without a delivery target are
// consumed by processing_status and stay untouched.
func TestExternalAgentArchiveOnlyReportsWithdrawnNotifications(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive pending scope", "pending-scope-daemon")
	legacy := dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
		"workspace_id":       testWorkspaceID,
		"recipient_agent_id": agent,
		"recipient_type":     "agent",
		"source_type":        "ci_run",
		"source_key":         "archive:legacy:" + uuidToString(dbid.NewV7()),
		"kind":               "ci_terminal",
		"payload":            testutil.Raw("'{\"run_id\":\"legacy\"}'::jsonb"),
		"delivery_status":    "pending",
		"last_error":         "legacy delivery has no registered external session",
	})
	code, resp, body := archivePreflightCall(t, agent)
	if code != http.StatusOK || !resp.Archivable {
		t.Fatalf("untargeted pending row should not claim a withdrawal: %d %+v %s", code, resp, body)
	}
	if _, ok := blockersByCode(resp.Blockers)["pending_messages"]; ok {
		t.Fatalf("untargeted pending row reported as withdrawable: %+v", resp.Blockers)
	}
	testutil.Call(t, testHandler.ArchiveAgent, withURLParam(
		newRequest(http.MethodPost, "/api/agents/"+agent+"/archive", nil), "id", agent)).Want(http.StatusOK)
	var withdrawn bool
	dbfx.QueryRow(t, "SELECT withdrawn_at IS NOT NULL FROM agent_mailbox_message WHERE id=$1", legacy).Scan(&withdrawn)
	if withdrawn {
		t.Fatal("archive claimed to withdraw a legacy row it leaves pending")
	}
}

// The coordinator write path takes the Agent reference lock, so an Agent
// archived before the write is refused instead of becoming a stranded
// coordinator.
func TestExternalAgentArchiveBlocksNewCoordinator(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive coordinator", "coordinator-daemon")
	issue := dbfx.Issue(t, "Coordinator target")
	coordinator := map[string]any{"coordinator": map[string]any{"type": "agent", "id": agent}}
	dbfx.Exec(t, "UPDATE agent SET archived_at=now() WHERE id=$1", agent)

	testutil.Call(t, testHandler.UpdateIssue, withURLParam(
		newRequest(http.MethodPut, "/api/issues/"+issue, coordinator), "id", issue)).Want(http.StatusConflict)
	if n := dbfx.Count(t, "SELECT count(*) FROM issue WHERE id=$1 AND coordinator_id=$2", issue, agent); n != 0 {
		t.Fatal("archived agent became the issue coordinator")
	}

	created := map[string]any{"title": "Coordinator create", "coordinator": map[string]any{"type": "agent", "id": agent}}
	testutil.Call(t, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", created)).Want(http.StatusConflict)
	if n := dbfx.Count(t, "SELECT count(*) FROM issue WHERE title='Coordinator create'"); n != 0 {
		t.Fatal("create stored an archived coordinator")
	}
}

// Adding a squad member takes the same lock, so a squad membership cannot land
// behind the archive's final reference scan.
func TestExternalAgentArchiveBlocksNewSquadMember(t *testing.T) {
	agent, _, _ := externalAgentFixture(t, "Archive squad member", "squad-member-daemon")
	leader := dbfx.Agent(t, "Squad member managed leader", testRuntimeID)
	squad := dbfx.Squad(t, "Squad member archive squad", leader)
	dbfx.Exec(t, "UPDATE agent SET archived_at=now() WHERE id=$1", agent)

	testutil.Call(t, testHandler.AddSquadMember, squadScopeReq("", http.MethodPost, "/api/squads/"+squad+"/members", map[string]any{
		"member_type": "agent",
		"member_id":   agent,
	}, map[string]string{"id": squad})).Want(http.StatusConflict)
	if n := dbfx.Count(t, "SELECT count(*) FROM squad_member WHERE squad_id=$1 AND member_type='agent' AND member_id=$2", squad, agent); n != 0 {
		t.Fatal("archived agent became a squad member")
	}
}
