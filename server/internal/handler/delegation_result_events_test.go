package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type committedResultNotification struct {
	managed, external, refused bool
	err                        error
}

func observeCommittedResultNotifications(t *testing.T, result db.AgentTaskQueue, address db.IssueDelegationReturn) <-chan committedResultNotification {
	t.Helper()
	observed := make(chan committedResultNotification, 8)
	testHandler.Bus.Subscribe(protocol.EventIssueMailboxChanged, func(event events.Event) {
		payload, ok := event.Payload.(map[string]any)
		if !ok || event.WorkspaceID != uuidToString(address.WorkspaceID) || payload["issue_id"] != uuidToString(result.IssueID) {
			return
		}
		var state committedResultNotification
		// The pool acquires outside the publisher's transaction: uncommitted
		// pointer/row writes cannot satisfy this observation.
		state.err = testPool.QueryRow(context.Background(), `SELECT
		  EXISTS(SELECT 1 FROM agent_task_queue task WHERE task.id=link.notification_task_id),
		  EXISTS(SELECT 1 FROM agent_mailbox_message delivery WHERE delivery.id=link.notification_delivery_id),
		  link.notification_error IS NOT NULL
		  FROM agent_task_delegation_return link WHERE task_id=$1 AND delegation_return_id=$2`, result.ID, address.ID).
			Scan(&state.managed, &state.external, &state.refused)
		observed <- state
	})
	return observed
}

func resultNotificationEventFixture(t *testing.T, kind string) (db.AgentTaskQueue, db.IssueDelegationReturn) {
	t.Helper()
	if kind != "external" {
		source, result, address := resultFixture(t, "refresh-"+kind)
		if kind == "refused" {
			dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, source.AgentID)
		}
		return result, address
	}
	dispatcher, sessionID, ownerID := externalAgentFixture(t, "result refresh", "refresh-daemon")
	token := externalTokenFixture(t, dispatcher, ownerID, time.Now().Add(time.Hour))
	target, _ := executableTarget(t, "refresh target")
	title := fmt.Sprintf("external result refresh %d", time.Now().UnixNano())
	bumpIssueCounter(t)
	w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.CreateIssue), http.MethodPost, "/api/issues",
		createIssueBody(title, target), nil, func(r *http.Request) { r.Header.Set(ReturnSessionHeader, sessionID) })
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	issueID, _ := issueIDByTitle(t, title)
	cleanupDelegatedIssue(t, issueID)
	result, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(latestTaskIDFor(t, issueID, target)))
	if err != nil {
		t.Fatal(err)
	}
	address, ok := acceptedReturn(t, issueID)
	if !ok {
		t.Fatal("missing accepted return")
	}
	return result, address
}

func TestDelegationResultRefreshFollowsCommit(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, kind := range []string{"managed", "external", "refused"} {
		t.Run(kind, func(t *testing.T) {
			result, address := resultNotificationEventFixture(t, kind)
			finishResultFixture(t, result)
			observed := observeCommittedResultNotifications(t, result, address)
			fn := "reject_result_event_" + strings.ReplaceAll(uuidToString(result.ID), "-", "_")
			// A deferred trigger fails COMMIT after the notification write.
			dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN
			  IF NEW.task_id='%s' AND (NEW.notification_task_id IS NOT NULL OR NEW.notification_delivery_id IS NOT NULL OR NEW.notification_error IS NOT NULL)
			  THEN RAISE EXCEPTION 'injected result notification commit failure'; END IF;
			  RETURN NEW; END $$ LANGUAGE plpgsql;
			  CREATE CONSTRAINT TRIGGER %s_trg AFTER UPDATE ON agent_task_delegation_return
			  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s();`, fn, uuidToString(result.ID), fn, fn))
			drop := func() {
				dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_delegation_return; DROP FUNCTION IF EXISTS %s();`, fn, fn))
			}
			t.Cleanup(drop)
			ctx := context.Background()
			stats, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100)
			if err != nil || stats.Materialized != 0 || stats.Refused != 0 {
				t.Fatalf("failed commit: stats=%+v err=%v", stats, err)
			}
			if len(observed) != 0 {
				t.Fatal("rolled-back notification published a refresh")
			}
			if pending := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL`, result.ID); pending != 1 {
				t.Fatal("failed commit changed the notification obligation")
			}
			drop()
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Fatal(err)
			}
			if len(observed) != 1 {
				t.Fatalf("committed notification refreshes=%d, want 1", len(observed))
			}
			state := <-observed
			if state.err != nil || state.managed != (kind == "managed") || state.external != (kind == "external") || state.refused != (kind == "refused") {
				t.Fatalf("subscriber could not read the committed notification: %+v", state)
			}
			if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
				t.Fatal(err)
			}
			if len(observed) != 0 {
				t.Fatal("unchanged notification published another refresh")
			}
		})
	}
}
