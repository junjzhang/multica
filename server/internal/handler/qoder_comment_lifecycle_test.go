package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Pause after the task transition locks its row, before Qoder synchronizes the
// issue. Deletion must wait on the earlier issue lock, not hold it while waiting
// on the task, which would form a PostgreSQL deadlock when the gate opens.
type qoderTransitionBarrier struct {
	pool    *pgxpool.Pool
	ready   chan int32
	proceed <-chan struct{}
}

func (s *qoderTransitionBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &qoderTransitionBarrierTx{Tx: tx, gate: s}, nil
}

type qoderTransitionBarrierTx struct {
	pgx.Tx
	gate *qoderTransitionBarrier
}

func (t *qoderTransitionBarrierTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: LockQoderTaskIssue :one") {
		t.gate.ready <- int32(t.Tx.Conn().PgConn().PID())
		select {
		case <-t.gate.proceed:
		case <-ctx.Done():
		}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func TestQoderCommentDeletionSerializesWithTaskTransition(t *testing.T) {
	for _, transition := range []string{"start", "claim_start", "complete"} {
		t.Run(transition, func(t *testing.T) {
			b := newAcceptedCommentBatch(t)
			dbfx.Exec(t, `UPDATE agent_runtime SET provider='qoder_cloud' WHERE id=$1`, b.runtime)
			dbfx.Exec(t, `UPDATE issue SET assignee_type='agent', assignee_id=$2 WHERE id=$1`, b.issue, b.target)
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched', started_at=NULL, dispatched_at=now() WHERE id=$1`, b.task)
			if transition == "complete" {
				if _, err := testHandler.TaskService.StartTask(context.Background(), parseUUID(b.task)); err != nil {
					t.Fatal(err)
				}
			}
			// Exercise the real provider, assignee and latest-run predicates.
			eligible, err := testHandler.Queries.LockQoderTaskIssue(context.Background(), parseUUID(b.task))
			if err != nil || uuidToString(eligible.ID) != b.issue {
				t.Fatalf("Qoder issue guard: %+v %v", eligible, err)
			}
			task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(b.task))
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ready, proceed := make(chan int32, 1), make(chan struct{})
			defer func() {
				select {
				case <-proceed:
				default:
					close(proceed)
				}
			}()
			svc := service.NewTaskService(testHandler.Queries,
				&qoderTransitionBarrier{pool: testPool, ready: ready, proceed: proceed},
				testHandler.TaskService.Hub, testHandler.TaskService.Bus)
			transitioned := make(chan error, 1)
			go func() {
				var err error
				switch transition {
				case "start":
					_, err = svc.StartTask(ctx, task.ID)
				case "claim_start":
					_, err = svc.StartTaskForClaim(ctx, db.LockAgentTaskStartClaimParams{ID: task.ID, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt})
				case "complete":
					_, _, err = svc.CompleteTaskWithTransition(ctx, task.ID, []byte(`{"output":"done"}`), "", "", "", false, "", "")
				}
				transitioned <- err
			}()
			var holderPID int32
			select {
			case holderPID = <-ready:
			case err := <-transitioned:
				t.Fatalf("transition ended before Qoder sync: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			deleted := make(chan int, 1)
			go func() {
				w := serveAuthenticated(t, b.token, http.HandlerFunc(testHandler.DeleteComment), http.MethodDelete,
					"/api/workspaces/"+testWorkspaceID+"/comments/"+b.second, nil, map[string]string{"commentId": b.second}, nil)
				deleted <- w.Code
			}()
			waitForBackendBlockedBy(t, holderPID)
			close(proceed)
			select {
			case err := <-transitioned:
				if err != nil {
					t.Errorf("Qoder transition failed: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case code := <-deleted:
				if code != http.StatusNoContent {
					t.Errorf("concurrent deletion returned HTTP %d", code)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if n := countValue(t, `SELECT count(*) FROM comment WHERE id=$1`, b.second); n != 0 {
				t.Fatal("deleted comment remains")
			}
			if transition == "complete" {
				if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='completed'`, b.task); n != 1 {
					t.Fatal("completion was lost")
				}
				if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, b.issue); n != 1 {
					t.Fatal("completed input was replayed")
				}
			} else if b.liveCoverage(t, b.first) != 1 || b.liveCoverage(t, b.second) != 0 {
				t.Fatal("deletion lost the surviving comment or replayed the removed input")
			}
		})
	}
}
