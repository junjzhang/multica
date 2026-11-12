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

	"github.com/jackc/pgx/v5/pgtype"
)

func TestDelegationAssignmentFailureRollsBackCloseWithdrawal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, _, token := managedDispatcherFixture(t, "close-assignment")
	target, _ := executableTarget(t, "close-assignment-target")
	issueID := dbfx.Issue(t, "Atomic close and assignment")
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	recipient, session, _ := externalAgentFixture(t, "Close recipient", "close-assignment-daemon")
	bindExternalSession(t, recipient, session, issueID)
	w := httptest.NewRecorder()
	testHandler.CreateComment(w, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments",
		map[string]any{"content": fmt.Sprintf("[@Recipient](mention://agent/%s) pending request", recipient)}), "id", issueID))
	if w.Code != http.StatusCreated {
		t.Fatalf("notification comment = %d: %s", w.Code, w.Body.String())
	}
	var comment CommentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &comment); err != nil {
		t.Fatal(err)
	}

	fn := "fail_assignment_intent_" + strings.ReplaceAll(issueID, "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN IF NEW.issue_id = '%s' THEN RAISE EXCEPTION 'injected assignment intent failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON issue_assignment_delegation FOR EACH ROW EXECUTE FUNCTION %s();`, fn, issueID, fn, fn))
	drop := func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON issue_assignment_delegation; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	}
	t.Cleanup(drop)
	update := func() *httptest.ResponseRecorder {
		return serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPatch,
			"/api/issues/"+issueID, map[string]any{"status": "cancelled", "assignee_type": "agent", "assignee_id": target}, map[string]string{"id": issueID}, nil)
	}
	w = update()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("failed assignment = %d: %s", w.Code, w.Body.String())
	}
	var status, assignee, delivery string
	var withdrawn pgtype.Timestamptz
	readState := func() {
		t.Helper()
		if err := testPool.QueryRow(context.Background(), `SELECT status, COALESCE(assignee_id::text, '') FROM issue WHERE id=$1`, issueID).Scan(&status, &assignee); err != nil {
			t.Fatal(err)
		}
		if err := testPool.QueryRow(context.Background(), `SELECT delivery_status, withdrawn_at FROM agent_mailbox_message WHERE comment_id=$1`, comment.ID).Scan(&delivery, &withdrawn); err != nil {
			t.Fatal(err)
		}
	}
	readState()
	if status != "todo" || assignee != "" || delivery != "pending" || withdrawn.Valid {
		t.Fatalf("rollback leaked state: status=%s assignee=%s delivery=%s withdrawn=%v", status, assignee, delivery, withdrawn.Valid)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issueID); n != 0 {
		t.Fatalf("rollback left %d accepted returns", n)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_assignment_delegation WHERE issue_id=$1`, issueID); n != 0 {
		t.Fatalf("rollback left %d assignments", n)
	}

	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID); n != 0 {
		t.Fatalf("rollback left %d executable runs", n)
	}

	drop()
	w = update()
	if w.Code != http.StatusOK {
		t.Fatalf("close and assignment = %d: %s", w.Code, w.Body.String())
	}
	readState()
	if status != "cancelled" || assignee != target || delivery == "pending" || delivery == "leased" || !withdrawn.Valid {
		t.Fatalf("commit lost close state: status=%s assignee=%s delivery=%s withdrawn=%v", status, assignee, delivery, withdrawn.Valid)
	}
	if n := countValue(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issueID); n != 1 {
		t.Fatalf("committed accepted returns = %d", n)
	}
	if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID); n != 1 {
		t.Fatalf("explicit assignment created %d runs", n)
	}
	run := latestTaskIDFor(t, issueID, target)
	if n := countValue(t, `SELECT count(*) FROM issue_assignment_delegation WHERE issue_id=$1 AND task_id=$2`, issueID, run); n != 1 {
		t.Fatalf("assignment did not bind its exact run: %d", n)
	}
	requireTaskReturns(t, run, acceptedReturns(t, issueID)...)

}

func TestDelegationAcceptedExternalReturnAppearsInBindingProjection(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dispatcher, session, owner := externalAgentFixture(t, "Delegation projection", "delegation-projection-daemon")
	token := externalTokenFixture(t, dispatcher, owner, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "delegation-projection-target")
	title := fmt.Sprintf("Accepted binding projection %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost,
		"/api/issues", createIssueBody(title, target), nil, func(r *http.Request) { r.Header.Set("X-Return-Session", session) })
	if w.Code != http.StatusCreated {
		t.Fatalf("create delegated issue = %d: %s", w.Code, w.Body.String())
	}
	issueID, ok := issueIDByTitle(t, title)
	if !ok {
		t.Fatal("delegated issue missing")
	}
	t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
	accepted, ok := acceptedReturn(t, issueID)
	if !ok || uuidToString(accepted.DispatcherAgentID) != dispatcher || uuidToString(accepted.ExternalSessionID) != session {
		t.Fatalf("accepted return = %+v", accepted)
	}
	requireTaskReturns(t, latestTaskIDFor(t, issueID, target), accepted)

	w = httptest.NewRecorder()
	testHandler.GetIssue(w, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID, nil), "id", issueID))
	if w.Code != http.StatusOK {
		t.Fatalf("read delegated issue = %d: %s", w.Code, w.Body.String())
	}
	var issue IssueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if issue.ExternalSessionBindings == nil || len(*issue.ExternalSessionBindings) != 1 {
		t.Fatalf("binding projection = %+v", issue.ExternalSessionBindings)
	}
	binding := (*issue.ExternalSessionBindings)[0]
	if binding.AgentID != dispatcher || binding.IssueID != issueID || binding.Session == nil || binding.Session.ID != session {
		t.Fatalf("binding does not match accepted return: %+v", binding)
	}
}
