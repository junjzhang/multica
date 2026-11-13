package handler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCommentObligationPoolAffinity(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, mode := range []string{"parent_originator", "denied", "owner_fallback", "fallback_refused", "fold", "active_plan"} {
		t.Run(mode, func(t *testing.T) {
			_, parentID, token := managedDispatcherFixture(t, "affinity-"+mode)
			other := dbfx.User(t, "target owner", fmt.Sprintf("affinity-%s-%d@example.test", mode, time.Now().UnixNano()))
			dbfx.Member(t, testWorkspaceID, other, "member")
			target, runtime := executableTarget(t, "affinity-target-"+mode)
			dbfx.Exec(t, `UPDATE agent SET owner_id=$2 WHERE id=$1`, target, other)
			dbfx.Exec(t, `UPDATE agent_runtime SET visibility='public' WHERE id=$1`, runtime)
			dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$3 WHERE id=$1`, parentID, testUserID, testUserID)
			issue := dbfx.Issue(t, "affinity "+mode)
			t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
			comment := pendingCommentObligation(t, "affinity", "", issue, token, target)
			if mode == "owner_fallback" || mode == "fallback_refused" {
				dbfx.Exec(t, `UPDATE comment SET source_task_id=NULL WHERE id=$1`, comment)
			} else {
				dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, target)
				permitted := testUserID
				if mode == "denied" {
					permitted = other
				}
				dbfx.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": target, "target_type": "member", "target_id": permitted})
			}
			if mode == "fallback_refused" {
				dbfx.Exec(t, `UPDATE workspace SET attribution_fail_closed=true WHERE id=$1`, testWorkspaceID)
				t.Cleanup(func() {
					dbfx.Exec(t, `UPDATE workspace SET attribution_fail_closed=false WHERE id=$1`, testWorkspaceID)
				})
			}
			existing := ""
			if mode == "fold" || mode == "active_plan" {
				status := "queued"
				if mode == "active_plan" {
					status = "running"
				}
				existing = dbfx.Task(t, target, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "trigger_comment_id": comment, "status": status, "originator_user_id": testUserID, "accountable_user_id": testUserID})
			}
			var obligation string
			dbfx.QueryRow(t, `SELECT id::text FROM comment_dispatch_obligation WHERE comment_id=$1`, comment).Scan(&obligation)
			config := testPool.Config().Copy()
			config.MaxConns = 4
			config.MinConns = 0
			application := "obligation-affinity-" + comment
			config.ConnConfig.RuntimeParams["application_name"] = application
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			h := New(db.New(pool), pool, testHandler.Hub, testHandler.Bus, nil, nil, nil, analytics.NoopClient{}, Config{})
			blocker, err := testPool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(t.Context(), `SELECT id FROM comment WHERE id=$1 FOR UPDATE`, comment); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			errs := make(chan error, 4)
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _, err := h.dispatchCommentObligation(ctx, parseUUID(obligation), false)
					errs <- err
				}()
			}
			deadline := time.Now().Add(3 * time.Second)
			waiting := 0
			for time.Now().Before(deadline) {
				if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'`, application).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting == 4 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if waiting != 4 {
				cancel()
				_ = blocker.Rollback(context.Background())
				wg.Wait()
				t.Fatalf("barrier did not put all four consumers in transactions: %d", waiting)
			}
			t.Logf("four consumers hold all %d pool connections before releasing comment lock", pool.Config().MaxConns)
			if err := blocker.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("bounded consumer failed: %v", err)
				}
			}
			if ctx.Err() != nil {
				t.Fatalf("consumers exhausted their bounded context: %v", ctx.Err())
			}
			want := 1
			if mode == "denied" || mode == "fallback_refused" {
				want = 0
			}
			count := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND trigger_comment_id=$2`, target, comment)
			if count != want {
				t.Fatalf("runs=%d want%d", count, want)
			}
			if want == 0 {
				return
			}
			taskID := latestTaskIDFor(t, issue, target)
			if existing != "" && taskID != existing {
				t.Fatal("coverage rebuilt the existing run")
			}
			task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "owner_fallback" {
				if task.OriginatorUserID.Valid || uuidToString(task.AccountableUserID) != other || task.OriginatorSource.String != "owner_fallback" {
					t.Fatalf("fallback attribution changed: source=%s", task.OriginatorSource.String)
				}
			} else if mode != "active_plan" {
				if uuidToString(task.OriginatorUserID) != testUserID || uuidToString(task.AccountableUserID) != testUserID || uuidToString(task.DelegatedFromTaskID) != parentID || uuidToString(task.TriggerEvidenceRefID) != comment {
					t.Fatal("parent attribution or comment evidence changed")
				}
			}
			requireTaskReturns(t, taskID, acceptedReturns(t, issue)...)
		})
	}
}
