package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestRegisteredSessionResponseCanBeUsedToBind(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	daemonID := "registration-contract-" + suffix
	dbfx.Runtime(t, "Registration contract runtime", testutil.Cols{"daemon_id": daemonID})
	hostSessionID := "host-session-" + suffix
	var registered struct {
		ID      string `json:"id"`
		Session struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		} `json:"external_session"`
	}
	testutil.Call(t, testHandler.CreateExternalAgent, newRequest(http.MethodPost, "/api/agents/external", map[string]any{
		"name":             "Registration contract " + suffix,
		"external_session": externalSessionBody("contract-host-"+suffix, hostSessionID, "Contract session", daemonID),
	})).Want(http.StatusCreated).JSON(&registered)
	dbfx.Cleanup(t, "DELETE FROM agent WHERE id=$1", registered.ID)
	dbfx.Cleanup(t, "DELETE FROM external_agent_session WHERE agent_id=$1", registered.ID)
	dbfx.Cleanup(t, "DELETE FROM external_agent_session_binding WHERE agent_id=$1", registered.ID)
	if registered.Session.ID == "" {
		t.Fatal("registration did not expose the session id required by the binding API")
	}
	if registered.Session.SessionID != hostSessionID || registered.Session.ID == hostSessionID {
		t.Fatalf("registered and host session identities were conflated: %+v", registered.Session)
	}
	issueID := dbfx.Issue(t, "Bind from registration response")
	request := withURLParam(newRequest(http.MethodPost, "/api/agents/"+registered.ID+"/session-bindings", map[string]any{
		"issue_id": issueID, "session_id": registered.Session.ID,
	}), "id", registered.ID)
	var binding struct {
		SessionID string `json:"session_id"`
	}
	testutil.Call(t, testHandler.BindAgentSession, request).Want(http.StatusCreated).JSON(&binding)
	if binding.SessionID != registered.Session.ID {
		t.Fatalf("binding target = %q, want registered session %q", binding.SessionID, registered.Session.ID)
	}
}
