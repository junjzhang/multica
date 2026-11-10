package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Slice 1: a host instance is one template; sessions attach to it. These tests
// pin the registration key, the repeat/concurrent cases, the rollout switch and
// the legacy immutability rule.

type templateRegistrationResponse struct {
	ID               string                         `json:"id"`
	ExternalSession  *ExternalSessionResponse       `json:"external_session"`
	ExternalTemplate *ExternalAgentTemplateResponse `json:"external_template"`
}

func templatesEnabledHandler(t *testing.T) *Handler {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	h := *testHandler
	h.cfg.ExternalAgentTemplatesEnabled = true
	return &h
}

func registerExternal(t *testing.T, h *Handler, body map[string]any) (int, templateRegistrationResponse, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.CreateExternalAgent(rec, newRequest(http.MethodPost, "/api/agents/external", body))
	var resp templateRegistrationResponse
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
	}
	return rec.Code, resp, rec.Body.String()
}

func templateBody(name, hostID, sessionID, displayName, daemonID string) map[string]any {
	return map[string]any{
		"name":             name,
		"template":         true,
		"external_session": externalSessionBody(hostID, sessionID, displayName, daemonID),
	}
}

func cleanupExternalAgent(t *testing.T, agentID string) {
	t.Helper()
	dbfx.Cleanup(t, "DELETE FROM external_agent_token WHERE agent_id=$1", agentID)
	dbfx.Cleanup(t, "DELETE FROM external_agent_session_binding WHERE agent_id=$1", agentID)
	dbfx.Cleanup(t, "DELETE FROM external_agent_session WHERE agent_id=$1", agentID)
	dbfx.Cleanup(t, "DELETE FROM agent WHERE id=$1", agentID)
}

func TestExternalTemplateRegistrationReusesHostTemplate(t *testing.T) {
	h := templatesEnabledHandler(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemonID := "template-daemon-" + suffix
	dbfx.Runtime(t, "Template daemon "+suffix, testutil.Cols{"daemon_id": daemonID})
	hostID := "template-host-" + suffix
	name := "Paseo · " + hostID

	code, first, _ := registerExternal(t, h, templateBody(name, hostID, "session-a-"+suffix, "Session A", daemonID))
	if code != http.StatusCreated {
		t.Fatalf("register first session: %d", code)
	}
	defer cleanupExternalAgent(t, first.ID)
	if first.ExternalTemplate == nil || first.ExternalTemplate.ID != first.ID || first.ExternalTemplate.HostID != hostID {
		t.Fatalf("first session did not register a host template: %+v", first.ExternalTemplate)
	}
	if first.ExternalSession == nil || first.ExternalSession.ID == "" {
		t.Fatal("registration must expose the registered session id")
	}

	// A second session of the same host attaches to the same template.
	code, second, _ := registerExternal(t, h, templateBody(name, hostID, "session-b-"+suffix, "Session B", daemonID))
	if code != http.StatusCreated {
		t.Fatalf("register second session: %d", code)
	}
	if second.ID != first.ID {
		t.Fatalf("second session created a new agent %s, want template %s", second.ID, first.ID)
	}
	if second.ExternalSession == nil || second.ExternalSession.ID == first.ExternalSession.ID {
		t.Fatalf("second session must be a distinct registered session: %+v", second.ExternalSession)
	}

	// Repeating one session returns its original identity and does not grow rows.
	code, repeat, _ := registerExternal(t, h, templateBody(name, hostID, "session-a-"+suffix, "Session A renamed", daemonID))
	if code != http.StatusOK {
		t.Fatalf("repeat registration: %d", code)
	}
	if repeat.ID != first.ID || repeat.ExternalSession.ID != first.ExternalSession.ID {
		t.Fatalf("repeat registration changed identity: %+v", repeat)
	}

	if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE workspace_id=$1 AND external_provider='paseo' AND external_host_id=$2`, testWorkspaceID, hostID); n != 1 {
		t.Fatalf("template rows for one host = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM external_agent_session WHERE agent_id=$1`, first.ID); n != 2 {
		t.Fatalf("sessions attached to template = %d, want 2", n)
	}
}

func TestExternalTemplateConcurrentFirstConnectCreatesOneTemplate(t *testing.T) {
	h := templatesEnabledHandler(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemonID := "template-concurrent-" + suffix
	dbfx.Runtime(t, "Concurrent template daemon "+suffix, testutil.Cols{"daemon_id": daemonID})
	hostID := "concurrent-host-" + suffix
	name := "Paseo · " + hostID

	const sessions = 4
	type result struct {
		code int
		resp templateRegistrationResponse
		body string
	}
	results := make(chan result, sessions)
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, resp, body := registerExternal(t, h, templateBody(name, hostID, fmt.Sprintf("concurrent-session-%d-%s", i, suffix), fmt.Sprintf("Session %d", i), daemonID))
			results <- result{code: code, resp: resp, body: body}
		}(i)
	}
	wg.Wait()
	close(results)

	agentID := ""
	for res := range results {
		if res.code != http.StatusCreated {
			t.Fatalf("concurrent first connect: %d %s", res.code, res.body)
		}
		if agentID == "" {
			agentID = res.resp.ID
			defer cleanupExternalAgent(t, agentID)
		}
		if res.resp.ID != agentID {
			t.Fatalf("concurrent first connects produced two templates: %s and %s", agentID, res.resp.ID)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE workspace_id=$1 AND external_provider='paseo' AND external_host_id=$2`, testWorkspaceID, hostID); n != 1 {
		t.Fatalf("concurrent template rows = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM external_agent_session WHERE agent_id=$1`, agentID); n != sessions {
		t.Fatalf("concurrent sessions = %d, want %d", n, sessions)
	}
}

func TestExternalTemplateRegistrationIsGatedByDefault(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemonID := "template-gate-" + suffix
	dbfx.Runtime(t, "Gated template daemon "+suffix, testutil.Cols{"daemon_id": daemonID})
	hostID := "gated-host-" + suffix
	sessionID := "gated-session-" + suffix

	// Template writes are off by default: the request fails closed and writes
	// nothing.
	code, _, _ := registerExternal(t, testHandler, templateBody("Paseo · "+hostID, hostID, sessionID, "Gated session", daemonID))
	if code != http.StatusBadRequest {
		t.Fatalf("template registration with the switch off = %d, want 400", code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE workspace_id=$1 AND external_host_id=$2`, testWorkspaceID, hostID); n != 0 {
		t.Fatalf("gated template registration wrote %d agent rows", n)
	}

	// Legacy per-session registration keeps working while the switch is off.
	code, legacy, _ := registerExternal(t, testHandler, map[string]any{
		"name":             "Legacy " + suffix,
		"external_session": externalSessionBody(hostID, sessionID, "Legacy session", daemonID),
	})
	if code != http.StatusCreated {
		t.Fatalf("legacy registration with the switch off = %d", code)
	}
	defer cleanupExternalAgent(t, legacy.ID)
	if legacy.ExternalTemplate != nil {
		t.Fatalf("legacy registration minted a template: %+v", legacy.ExternalTemplate)
	}
}

func TestExternalTemplateRegistrationKeepsLegacySessionIdentity(t *testing.T) {
	h := templatesEnabledHandler(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemonID := "template-legacy-" + suffix
	dbfx.Runtime(t, "Legacy identity daemon "+suffix, testutil.Cols{"daemon_id": daemonID})
	hostID := "legacy-host-" + suffix
	sessionID := "legacy-session-" + suffix

	// A session registered before templates existed keeps its own agent.
	code, legacy, _ := registerExternal(t, h, map[string]any{
		"name":             "Legacy identity " + suffix,
		"external_session": externalSessionBody(hostID, sessionID, "Legacy session", daemonID),
	})
	if code != http.StatusCreated {
		t.Fatalf("legacy registration: %d", code)
	}
	defer cleanupExternalAgent(t, legacy.ID)

	// A later template-mode connect for the SAME session returns that identity
	// and does not move the session onto a template.
	code, repeat, _ := registerExternal(t, h, templateBody("Paseo · "+hostID, hostID, sessionID, "Legacy session", daemonID))
	if code != http.StatusOK {
		t.Fatalf("template-mode repeat of a legacy session: %d", code)
	}
	if repeat.ID != legacy.ID || repeat.ExternalTemplate != nil {
		t.Fatalf("legacy session was absorbed into a template: %+v", repeat)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE workspace_id=$1 AND external_host_id=$2`, testWorkspaceID, hostID); n != 0 {
		t.Fatalf("template-mode repeat created %d template rows", n)
	}
}

func TestExternalTokenCreationBindsExactlyOneSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionA, owner := externalAgentFixture(t, "Token binding", "token-binding-daemon")
	defer cleanupExternalAgent(t, agentID)
	sessionB := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "owner_user_id": owner,
		"provider": "paseo", "host_id": "token-binding-second", "session_id": "token-binding-second-session", "daemon_id": "token-binding-daemon",
	})
	expires := time.Now().Add(time.Hour).Format(time.RFC3339)

	mint := func(body map[string]any) (int, map[string]any) {
		rec := httptest.NewRecorder()
		testHandler.CreateExternalAgentToken(rec, withURLParam(newRequest(http.MethodPost, "/api/agents/"+agentID+"/tokens", body), "id", agentID))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// An ambiguous agent cannot mint a wide credential.
	if code, _ := mint(map[string]any{"expires_at": expires}); code != http.StatusConflict {
		t.Fatalf("session-less token for a two-session agent = %d, want 409", code)
	}
	code, out := mint(map[string]any{"expires_at": expires, "external_session_id": sessionA})
	if code != http.StatusCreated {
		t.Fatalf("session-bound token = %d: %v", code, out)
	}
	if out["external_session_id"] != sessionA {
		t.Fatalf("token bound to %v, want %s", out["external_session_id"], sessionA)
	}
	code, out = mint(map[string]any{"expires_at": expires, "external_session_id": sessionB})
	if code != http.StatusCreated || out["external_session_id"] != sessionB {
		t.Fatalf("second session token = %d: %v", code, out)
	}
}

func TestExternalSessionCredentialIsolatesMailboxAndReturnParam(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionA, owner := externalAgentFixture(t, "Credential isolation", "credential-isolation-daemon")
	defer cleanupExternalAgent(t, agentID)
	sessionB := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "owner_user_id": owner,
		"provider": "paseo", "host_id": "credential-isolation-second", "session_id": "credential-isolation-second-session", "daemon_id": "credential-isolation-daemon",
	})
	hostSessionA := ""
	if err := testPool.QueryRow(t.Context(), `SELECT session_id FROM external_agent_session WHERE id=$1`, sessionA).Scan(&hostSessionA); err != nil {
		t.Fatal(err)
	}
	tokenA := externalSessionTokenFixture(t, agentID, sessionA, owner, time.Now().Add(time.Hour))
	tokenB := externalSessionTokenFixture(t, agentID, sessionB, owner, time.Now().Add(time.Hour))

	issueID := dbfx.Issue(t, "credential isolation mailbox")
	commentID := dbfx.Comment(t, issueID, "request for B")
	dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "comment_id": commentID,
		"recipient_agent_id": agentID, "target_session_id": sessionB,
		"sender_type": "member", "sender_id": testUserID,
	})

	list := func(token, returnSession string) *httptest.ResponseRecorder {
		return serveAuthenticated(t, token, http.HandlerFunc(testHandler.ListAgentMailbox), http.MethodGet, "/api/agents/"+agentID+"/mailbox", nil, map[string]string{"id": agentID}, func(r *http.Request) {
			if returnSession != "" {
				r.Header.Set(ReturnSessionHeader, returnSession)
			}
		})
	}
	var page struct {
		Messages []MailboxMessageResponse `json:"messages"`
	}
	rec := list(tokenA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list as session A: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 0 {
		t.Fatalf("session A saw %d of session B's messages", len(page.Messages))
	}
	rec = list(tokenB, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), commentID) {
		t.Fatalf("session B cannot see its own message: %d %s", rec.Code, rec.Body.String())
	}

	// The header is a consistency parameter: naming another session is refused
	// before the handler, so no read or write happens.
	if rec := list(tokenA, sessionB); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-session return header = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// Naming its own session through either id form is consistent.
	if rec := list(tokenA, hostSessionA); rec.Code != http.StatusOK {
		t.Fatalf("own provider session id = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestLegacyWideTokenIsGatedForSessionSensitiveOperations(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionA, owner := externalAgentFixture(t, "Legacy wide token", "legacy-wide-daemon")
	defer cleanupExternalAgent(t, agentID)
	issueID := dbfx.Issue(t, "legacy wide token mailbox")
	commentID := dbfx.Comment(t, issueID, "request")
	dbfx.Insert(t, "agent_mailbox_message", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "comment_id": commentID,
		"recipient_agent_id": agentID, "target_session_id": sessionA,
		"sender_type": "member", "sender_id": testUserID,
	})
	// A single-session legacy credential still works: the server can prove the
	// only session it can act as.
	legacyToken := externalTokenFixture(t, agentID, owner, time.Now().Add(time.Hour))
	rec := serveAuthenticated(t, legacyToken, http.HandlerFunc(testHandler.ListAgentMailbox), http.MethodGet, "/api/agents/"+agentID+"/mailbox", nil, map[string]string{"id": agentID}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("single-session legacy token: %d %s", rec.Code, rec.Body.String())
	}

	// Adding a second session makes that credential ambiguous: the wide token is
	// refused for session-sensitive work instead of guessing a sibling.
	dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "owner_user_id": owner,
		"provider": "paseo", "host_id": "legacy-wide-second", "session_id": "legacy-wide-second-session", "daemon_id": "legacy-wide-daemon",
	})
	rec = serveAuthenticated(t, legacyToken, http.HandlerFunc(testHandler.ListAgentMailbox), http.MethodGet, "/api/agents/"+agentID+"/mailbox", nil, map[string]string{"id": agentID}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ambiguous legacy token = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
