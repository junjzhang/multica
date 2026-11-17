package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestManagedMailboxPaginationSkipsInvisibleRows(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, _, first, _ := managedResultMailbox(t, "pagination-visible")
	token := taskTokenFixture(t, uuidToString(source.AgentID), uuidToString(source.ID))

	makeResult := func(name, callerToken string) string {
		t.Helper()
		target, _ := executableTarget(t, name)
		title := fmt.Sprintf("%s %d", name, time.Now().UnixNano())
		bumpIssueCounter(t)
		w := serveAuthenticated(t, callerToken, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues", createIssueBody(title, target), nil, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		issueID, _ := issueIDByTitle(t, title)
		cleanupDelegatedIssue(t, issueID)
		result, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(latestTaskIDFor(t, issueID, target)))
		if err != nil {
			t.Fatal(err)
		}
		finishResultFixture(t, result)
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
		rows, err := testHandler.Queries.ListTaskResultNotifications(ctx, db.ListTaskResultNotificationsParams{TaskID: result.ID, WorkspaceID: parseUUID(testWorkspaceID)})
		if err != nil || len(rows) != 1 || !rows[0].NotificationDeliveryID.Valid {
			t.Fatalf("result: %+v %v", rows, err)
		}
		return uuidToString(rows[0].NotificationDeliveryID)
	}
	otherIssue := dbfx.Issue(t, "pagination other conversation")
	otherID := dbfx.Task(t, uuidToString(source.AgentID), testutil.Cols{"issue_id": otherIssue, "runtime_id": uuidToString(source.RuntimeID), "status": "running", "session_id": "other-pagination-session", "work_dir": "/work/other-pagination", "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
	hidden := makeResult("pagination-other-scope", taskTokenFixture(t, uuidToString(source.AgentID), otherID))
	second := makeResult("pagination-second", token)
	// The same agent has a result in another conversation ahead of this page.
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET created_at=now()-interval '3 minutes' WHERE id=$1`, hidden)
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET created_at=now()-interval '2 minutes' WHERE id=$1`, first)
	dbfx.Exec(t, `UPDATE agent_mailbox_message SET created_at=now()-interval '1 minute' WHERE id=$1`, second)

	var after string
	for i, want := range []string{first, second} {
		path := "/mailbox?kind=delegation_result&limit=1&after=" + url.QueryEscape(after)
		response := serveAuthenticated(t, token, http.HandlerFunc(testHandler.ListAgentMailbox), http.MethodGet, path, nil, map[string]string{"id": uuidToString(source.AgentID)}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", i, response.Code, response.Body.String())
		}
		var page struct {
			Messages   []MailboxMessageResponse `json:"messages"`
			NextCursor *string                  `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) != 1 || page.Messages[0].ID != want || page.NextCursor == nil {
			t.Fatalf("page %d = %+v, want %s and cursor", i, page, want)
		}
		after = *page.NextCursor
	}
	response := serveAuthenticated(t, token, http.HandlerFunc(testHandler.ListAgentMailbox), http.MethodGet, "/mailbox?kind=delegation_result&limit=1&after="+url.QueryEscape(after), nil, map[string]string{"id": uuidToString(source.AgentID)}, nil)
	var last struct {
		Messages   []MailboxMessageResponse `json:"messages"`
		NextCursor *string                  `json:"next_cursor"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &last) != nil || len(last.Messages) != 0 || last.NextCursor != nil {
		t.Fatalf("last page: %d %s", response.Code, response.Body.String())
	}
}
