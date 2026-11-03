package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExternalRebindingDoesNotRetargetAcceptedResult(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	dispatcher, originalSession, ownerID := externalAgentFixture(t, "result rebind", "result-rebind-daemon")
	token := externalTokenFixture(t, dispatcher, ownerID, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "result-rebind-target")
	title := fmt.Sprintf("result rebind %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, func(r *http.Request) {
		r.Header.Set(ReturnSessionHeader, originalSession)
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	accepted, _ := acceptedReturn(t, issueID)
	result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, issueID, target)))
	if err != nil {
		t.Fatal(err)
	}
	replacement := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "owner_user_id": ownerID, "agent_id": dispatcher,
		"provider": "paseo", "host_id": "replacement-" + dispatcher,
		"session_id": "replacement-" + dispatcher, "display_name": "Replacement",
		"daemon_id": "result-rebind-daemon",
	})
	request := withURLParam(newRequestAs(ownerID, http.MethodPost, "/api/agents/"+dispatcher+"/session-bindings", map[string]any{
		"issue_id": issueID, "session_id": replacement,
	}), "id", dispatcher)
	testutil.Call(t, testHandler.BindAgentSession, request).Want(http.StatusCreated)

	content := fmt.Sprintf("[@Executor](mention://agent/%s) inspect the next revision", target)
	w = serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateComment), http.MethodPost,
		"/api/issues/"+issueID+"/comments", map[string]any{"content": content}, map[string]string{"id": issueID}, func(r *http.Request) {
			r.Header.Set(ReturnSessionHeader, replacement)
		})
	if w.Code != http.StatusConflict {
		t.Errorf("ordinary dispatch after rebind must require explicit result takeover: %d %s", w.Code, w.Body.String())
	}
	var comments int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND content=$2`, issueID, content).Scan(&comments)
	if comments != 0 {
		t.Errorf("rejected dispatch left %d comments", comments)
	}
	after, err := testHandler.Queries.GetDelegationReturnByID(ctx, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExternalSessionID != accepted.ExternalSessionID {
		t.Errorf("accepted return changed from %s to %s", originalSession, uuidToString(after.ExternalSessionID))
	}
	finishResultFixture(t, result)
	if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var count int
	var actual string
	dbfx.QueryRow(t, `SELECT count(*),COALESCE(min(m.target_session_id::text),'')
		FROM agent_mailbox_message m JOIN agent_task_delegation_return l ON l.notification_delivery_id=m.id
		WHERE l.task_id=$1`, result.ID).Scan(&count, &actual)
	if count != 1 || actual != originalSession {
		t.Fatalf("original result was retargeted: deliveries=%d target=%s want=%s", count, actual, originalSession)
	}
}
