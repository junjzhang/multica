package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// One agent never executes two runs on one issue at once, whatever triggered
// them: another comment thread, a delegation result or an untargeted trigger
// such as an assignment.
// Spare agent capacity must not open a second slot on the same issue.
func TestClaimSerializesEveryTriggerKindPerIssueAndAgent(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	svc := NewTaskService(queries, pool, nil, events.New())

	agentID := createClaimCapacityFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE agent SET max_concurrent_tasks = 10 WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	var issueID, runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT issue_id, runtime_id FROM agent_task_queue WHERE agent_id = $1 ORDER BY created_at, id LIMIT 1
	`, agentID).Scan(&issueID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	// Keep the fixture's other issue out of the way; only one issue is under test.
	if _, err := pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE agent_id = $1 AND issue_id <> $2`, agentID, issueID); err != nil {
		t.Fatal(err)
	}
	var threadA, threadB string
	for _, thread := range []*string{&threadA, &threadB} {
		if err := pool.QueryRow(ctx, `
			INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
			SELECT $1, workspace_id, 'member', creator_id, 'trigger', 'comment' FROM issue WHERE id = $1
			RETURNING id
		`, issueID).Scan(thread); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE agent_task_queue SET trigger_comment_id = $2 WHERE agent_id = $1
	`, agentID, threadA); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ thread, context any }{
		{threadB, `{}`},
		{nil, `{"type":"delegation_result"}`},
		{nil, `{}`},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, priority, context, trigger_comment_id)
			VALUES ($1, $2, $3, 'queued', 0, $4::text::jsonb, $5)
		`, agentID, issueID, runtimeID, row.context, row.thread); err != nil {
			t.Fatal(err)
		}
	}

	agent := util.MustParseUUID(agentID)
	claim := func() *db.AgentTaskQueue {
		t.Helper()
		task, err := svc.ClaimTask(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	for want := 4; want > 0; want-- {
		first := claim()
		if first == nil {
			t.Fatalf("no claimable run with %d queued", want)
		}
		if second := claim(); second != nil {
			t.Fatalf("claimed %s while %s is active on the same issue", util.UUIDToString(second.ID), util.UUIDToString(first.ID))
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, first.ID); err != nil {
			t.Fatal(err)
		}
		if second := claim(); second != nil {
			t.Fatalf("claimed %s while %s is running on the same issue", util.UUIDToString(second.ID), util.UUIDToString(first.ID))
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, first.ID); err != nil {
			t.Fatal(err)
		}
	}
	if last := claim(); last != nil {
		t.Fatalf("unexpected extra run %s", util.UUIDToString(last.ID))
	}
}
