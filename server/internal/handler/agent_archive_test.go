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
			"basis":              map[string]any{},
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
