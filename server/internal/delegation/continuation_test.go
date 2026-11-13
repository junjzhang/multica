package delegation

import (
	"context"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestManagedContinuationConcurrentAdmissionAndRollback(t *testing.T) {
	ctx := context.Background()
	agent, source := managedRun(t, managedRunOptions{name: "continuation-atomic", pinned: true})
	issue := dbfx.Issue(t, "continuation child")
	ensure := func(task string, rollback bool) (db.IssueDelegationReturn, error) {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			return db.IssueDelegationReturn{}, err
		}
		defer tx.Rollback(ctx)
		row, err := Ensure(ctx, testQueries.WithTx(tx), testWorkspaceID, issue, Address{DispatcherAgentID: agent, Kind: KindManagedTask, SourceTaskID: task}, "")
		if err == nil && !rollback {
			err = tx.Commit(ctx)
		}
		return row, err
	}
	old, err := ensure(source, false)
	if err != nil {
		t.Fatal(err)
	}
	original, err := testQueries.GetAgentTask(ctx, util.MustParseUUID(source))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed',completed_at=now() WHERE id=$1`, source)
	next := dbfx.Task(t, agent, testutil.Cols{
		"runtime_id": original.RuntimeID, "issue_id": original.IssueID, "status": "running",
		"session_id": "continued-atomic", "work_dir": "/work/continued-atomic",
		"resource_control_required": true, "session_pin_confirmed": true,
		"session_pin_generation": testutil.Raw("gen_random_uuid()"),
	})
	if _, err := ensure(next, true); err != nil {
		t.Fatal(err)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issue); n != 1 {
		t.Fatalf("rolled-back dispatch left %d versions", n)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='delegation_return_continued'`, issue); n != 0 {
		t.Fatalf("rolled-back dispatch left %d audits", n)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			row, err := ensure(next, false)
			if err != nil || row.ID == old.ID || row.SourceTaskID != util.MustParseUUID(next) {
				t.Errorf("concurrent admission: %+v %v", row, err)
			}
		})
	}
	wg.Wait()
	if n := countRowsIn(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1`, issue); n != 2 {
		t.Fatalf("concurrent admission created %d versions", n)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM issue_delegation_return WHERE issue_id=$1 AND is_current`, issue); n != 1 {
		t.Fatalf("current count = %d", n)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='delegation_return_continued'`, issue); n != 1 {
		t.Fatalf("continuation audit count = %d", n)
	}
}
