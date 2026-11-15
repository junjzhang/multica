package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestRerunReplacementRollsBackOnInsertFailure(t *testing.T) {
	svc, pool, creatorID, agentID, issueID, runtimeID := rerunQueueFixture(t)
	ctx := context.Background()
	pendingID := seedRerunTask(t, pool, agentID, runtimeID, issueID, "queued")
	fn := "reject_rerun_insert_" + strings.ReplaceAll(agentID, "-", "_")
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.agent_id = '%s' THEN RAISE EXCEPTION 'injected rerun insert failure'; END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER %s_trg BEFORE INSERT ON agent_task_queue
		FOR EACH ROW EXECUTE FUNCTION %s();`, fn, agentID, fn, fn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_trg ON agent_task_queue; DROP FUNCTION IF EXISTS %s();`, fn, fn)); err != nil {
			t.Errorf("remove insertion failpoint: %v", err)
		}
	})
	cancelEvents := 0
	svc.Bus.Subscribe(protocol.EventTaskCancelled, func(events.Event) { cancelEvents++ })
	_, err = svc.RerunIssue(ctx, util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), false, nil)
	if err == nil || !strings.Contains(err.Error(), "injected rerun insert failure") {
		t.Fatalf("expected injected enqueue failure, got %v", err)
	}
	if status := taskStatus(t, pool, pendingID); status != "queued" {
		t.Errorf("failed replacement changed the original task to %s", status)
	}
	if cancelEvents != 0 {
		t.Errorf("failed replacement emitted %d cancellation events", cancelEvents)
	}
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, agentID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Errorf("failed replacement left %d tasks, want the original task only", tasks)
	}
}

func TestConcurrentRerunsKeepOnePendingReplacement(t *testing.T) {
	svc, pool, creatorID, agentID, issueID, runtimeID := rerunQueueFixture(t)
	pendingID := seedRerunTask(t, pool, agentID, runtimeID, issueID, "queued")
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, issueID); err != nil {
			t.Errorf("remove rerun fixtures: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start, results := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), false, nil)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("concurrent rerun: %v", err)
		}
	}
	if status := taskStatus(t, pool, pendingID); status != "cancelled" {
		t.Errorf("original pending task remains %s", status)
	}
	var pending, total int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE status='queued'),count(*)
		FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, agentID).Scan(&pending, &total); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || total != 3 {
		t.Errorf("two accepted reruns left pending=%d total=%d, want one pending and three recorded attempts", pending, total)
	}
}
