package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDelegationResultRecoveryDoesNotStarveLaterRecipients(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	ctx := context.Background()
	source, result, _ := resultFixture(t, "fair-result-recovery")
	finishResultFixture(t, result)
	var healthyAgent string
	for i := 0; i < 100; i++ {
		agent := dbfx.Agent(t, fmt.Sprintf("result recovery recipient %d", i), uuidToString(source.RuntimeID), testutil.Cols{"execution_kind": "managed", "visibility": "workspace"})
		sourceID := dbfx.Task(t, agent, testutil.Cols{"issue_id": uuidToString(source.IssueID), "runtime_id": uuidToString(source.RuntimeID), "status": "running", "session_id": fmt.Sprintf("fair-session-%d", i), "work_dir": fmt.Sprintf("/fair-result/%d", i), "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": testutil.Raw("gen_random_uuid()")})
		address, err := delegation.ResolveManaged(ctx, testHandler.Queries, testWorkspaceID, agent, sourceID)
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := delegation.Ensure(ctx, testHandler.Queries, testWorkspaceID, uuidToString(result.IssueID), address, testUserID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := testHandler.Queries.LinkAgentTaskDelegationReturn(ctx, db.LinkAgentTaskDelegationReturnParams{TaskID: result.ID, DelegationReturnID: accepted.ID}); err != nil {
			t.Fatal(err)
		}
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now(),terminal_reported_at=now() WHERE id=$1`, sourceID)
		healthyAgent = agent
	}
	fn := "fail_result_recovery_" + strings.ReplaceAll(uuidToString(result.ID), "-", "_")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN IF NEW.issue_id='%s' AND NEW.context->>'type'='delegation_result' AND NEW.agent_id<>'%s' THEN RAISE EXCEPTION 'recipient-specific transient write failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql; CREATE TRIGGER %s BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s();`, fn, uuidToString(source.IssueID), healthyAgent, fn, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn))
	})
	for range 2 {
		if _, err := testHandler.TaskService.ReconcileDelegationResults(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND context->>'type'='delegation_result'`, healthyAgent)
	if n != 1 {
		t.Fatalf("first 100 retryable obligations starved later valid recipient: turns=%d", n)
	}
	pending := countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1 AND notification_error IS NULL AND notification_task_id IS NULL`, result.ID)
	if pending != 100 {
		t.Fatalf("retryable dependencies were discarded: pending=%d", pending)
	}
}
