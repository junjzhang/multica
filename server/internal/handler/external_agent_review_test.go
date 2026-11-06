package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestIssueCoordinatorMemberUsesUserIdentity pins the wire meaning of a member
// coordinator: id is the USER id, matching the create default and the CLI. A
// member row id is a different identity and must be rejected.
func TestIssueCoordinatorMemberUsesUserIdentity(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	memberID := dbfx.User(t, "Coordinator Member", "coordinator-member-"+suffix+"@test")
	memberRowID := dbfx.Member(t, testWorkspaceID, memberID, "member")
	strangerID := dbfx.User(t, "Coordinator Stranger", "coordinator-stranger-"+suffix+"@test")

	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":       "explicit member coordinator",
		"coordinator": map[string]any{"type": "member", "id": memberID},
	})
	testHandler.CreateIssue(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with explicit member coordinator: %d %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode created issue: %v", err)
	}
	if created.Coordinator == nil || *created.Coordinator == nil || (*created.Coordinator).Type != "member" || (*created.Coordinator).ID != memberID {
		t.Fatalf("create coordinator = %+v, want member/%s", created.Coordinator, memberID)
	}

	// Update to another member by user id succeeds and stores that same id.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"coordinator": map[string]any{"type": "member", "id": testUserID},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update to member coordinator: %d %s", w.Code, w.Body.String())
	}
	var updated IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated issue: %v", err)
	}
	if updated.Coordinator == nil || *updated.Coordinator == nil || (*updated.Coordinator).Type != "member" || (*updated.Coordinator).ID != testUserID {
		t.Fatalf("update coordinator = %+v, want member/%s", updated.Coordinator, testUserID)
	}
	var storedType, storedID string
	if err := testPool.QueryRow(context.Background(), `SELECT coordinator_type, coordinator_id FROM issue WHERE id = $1`, created.ID).Scan(&storedType, &storedID); err != nil {
		t.Fatalf("read coordinator: %v", err)
	}
	if storedType != "member" || storedID != testUserID {
		t.Fatalf("stored coordinator = %s/%s, want member/%s", storedType, storedID, testUserID)
	}

	// A member row id is not a user id and must not be accepted.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"coordinator": map[string]any{"type": "member", "id": memberRowID},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("member-row-id coordinator status = %d, want 400", w.Code)
	}

	// A user with no membership in this workspace is rejected on both create
	// and update.
	w = httptest.NewRecorder()
	r = newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":       "cross-workspace coordinator",
		"coordinator": map[string]any{"type": "member", "id": strangerID},
	})
	testHandler.CreateIssue(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-workspace coordinator create status = %d, want 400", w.Code)
	}
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPut, "/api/issues/"+created.ID, map[string]any{
		"coordinator": map[string]any{"type": "member", "id": strangerID},
	}), "id", created.ID)
	testHandler.UpdateIssue(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-workspace coordinator update status = %d, want 400", w.Code)
	}
}

// TestExternalAgentTokenIssuedByAdminBorrowsNoAuthority verifies that a token
// minted by a workspace admin for another member's external agent authenticates
// as that agent's OWNER, not as the admin, and dies when the owner loses
// workspace membership.
func TestExternalAgentTokenIssuedByAdminBorrowsNoAuthority(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, _, ownerID := externalAgentFixture(t, "Admin Issued Token", "daemon-admin-token")
	dbfx.Cleanup(t, `DELETE FROM external_agent_token WHERE agent_id = $1`, agentID)

	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/agents/"+agentID+"/tokens", map[string]any{
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}), "id", agentID)
	testHandler.CreateExternalAgentToken(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin-issued token: %d %s", w.Code, w.Body.String())
	}
	var token createExternalAgentTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if token.Token == "" {
		t.Fatal("token response carried no credential")
	}

	authW, headers := serveExternalAuth(t, token.Token, func(r *http.Request) {
		// Even a forged request that claims the admin identity must be
		// replaced by the credential's own owner.
		r.Header.Set("X-User-ID", testUserID)
		r.Header.Set("X-Actor-Source", "task_token")
	})
	if authW.Code != http.StatusOK {
		t.Fatalf("valid admin-issued token status = %d: %s", authW.Code, authW.Body.String())
	}
	if got := headers.Get("X-User-ID"); got != ownerID {
		t.Fatalf("X-User-ID = %q, want agent owner %s (not minting admin %s)", got, ownerID, testUserID)
	}
	if got := headers.Get("X-Actor-Source"); got != "external_agent" {
		t.Fatalf("X-Actor-Source = %q, want external_agent", got)
	}
	if got := headers.Get("X-Task-ID"); got != "" {
		t.Fatalf("X-Task-ID = %q, want empty for an external identity", got)
	}

	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, ownerID)
	authW, _ = serveExternalAuth(t, token.Token, nil)
	if authW.Code != http.StatusUnauthorized {
		t.Fatalf("token after owner membership loss status = %d, want 401", authW.Code)
	}
}

// TestExternalAgentRegistrationUsesMemberAccess covers the F6 clarification:
// an ordinary member may register their OWN host session, while managing or
// minting credentials for another member's agent still needs owner/admin.
func TestExternalAgentRegistrationUsesMemberAccess(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	memberID := dbfx.User(t, "External Session Owner", "external-session-owner-"+suffix+"@test")
	dbfx.Member(t, testWorkspaceID, memberID, "member")
	otherMemberID := dbfx.User(t, "External Session Other", "external-session-other-"+suffix+"@test")
	dbfx.Member(t, testWorkspaceID, otherMemberID, "member")

	daemonID := "daemon-member-" + suffix
	dbfx.Runtime(t, "Member External Daemon", testutil.Cols{
		"daemon_id":    daemonID,
		"provider":     "paseo",
		"runtime_mode": "cloud",
		"owner_id":     memberID,
	})
	body := map[string]any{
		"name": "Member External Session " + suffix,
		"external_session": map[string]any{
			"provider":     "paseo",
			"host_id":      "host-" + suffix,
			"session_id":   "session-" + suffix,
			"display_name": "Member Owned Session",
			"daemon_id":    daemonID,
		},
	}

	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/agents/external", body)
	r.Header.Set("X-User-ID", memberID)
	testHandler.CreateExternalAgent(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("member registering own session: %d %s", w.Code, w.Body.String())
	}
	var created AgentResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if created.ExecutionKind != "external" || created.ExternalSession == nil {
		t.Fatalf("registered agent = kind %q session %+v, want external with session", created.ExecutionKind, created.ExternalSession)
	}
	dbfx.Cleanup(t, `DELETE FROM agent WHERE id = $1`, created.ID)
	dbfx.Cleanup(t, `DELETE FROM external_agent_session WHERE agent_id = $1`, created.ID)
	dbfx.Cleanup(t, `DELETE FROM external_agent_token WHERE agent_id = $1`, created.ID)
	var ownerID string
	if err := testPool.QueryRow(context.Background(), `SELECT owner_id FROM agent WHERE id = $1`, created.ID).Scan(&ownerID); err != nil {
		t.Fatalf("read agent owner: %v", err)
	}
	if ownerID != memberID {
		t.Fatalf("agent owner = %s, want registering member %s", ownerID, memberID)
	}

	// Token management remains owner/admin: a different plain member cannot
	// mint for this agent.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPost, "/api/agents/"+created.ID+"/tokens", map[string]any{
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}), "id", created.ID)
	r.Header.Set("X-User-ID", otherMemberID)
	testHandler.CreateExternalAgentToken(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner member token issuance status = %d, want 403", w.Code)
	}

	// Losing membership revokes the right to register.
	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, memberID)
	w = httptest.NewRecorder()
	r = newRequest(http.MethodPost, "/api/agents/external", body)
	r.Header.Set("X-User-ID", memberID)
	testHandler.CreateExternalAgent(w, r)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("non-member registration status = %d, want 403 or 404", w.Code)
	}
}
