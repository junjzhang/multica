package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDelegationScopeReadFailuresRemainTransient(t *testing.T) {
	for _, kind := range []string{"scope-row", "scope-owner", "notification"} {
		t.Run(kind, func(t *testing.T) {
			svc, pool, _, agentID, issueID, runtimeID := rerunQueueFixture(t)
			sourceID := seedRerunTask(t, pool, agentID, runtimeID, issueID, "completed")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			source, err := svc.Queries.GetAgentTask(ctx, sourceID)
			if err != nil {
				t.Fatal(err)
			}
			actor, err := svc.Queries.GetAgent(ctx, util.MustParseUUID(agentID))
			if err != nil {
				t.Fatal(err)
			}
			address := db.IssueDelegationReturn{WorkspaceID: actor.WorkspaceID, DispatcherAgentID: actor.ID, ReturnKind: string(delegation.KindManagedTask), SourceTaskID: sourceID, ScopeTaskID: sourceID}
			source.IssueID = pgtype.UUID{}
			locker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer locker.Rollback(context.Background())
			switch kind {
			case "scope-row":
				_, err = locker.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE`, sourceID)
			case "scope-owner":
				_, err = locker.Exec(ctx, `LOCK TABLE agent IN ACCESS EXCLUSIVE MODE`)
			case "notification":
				_, err = locker.Exec(ctx, `LOCK TABLE agent_task_delegation_return IN ACCESS EXCLUSIVE MODE`)
			}
			if err != nil {
				t.Fatal(err)
			}
			reader, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Rollback(context.Background())
			if _, err = reader.Exec(ctx, `SET LOCAL lock_timeout='80ms'`); err != nil {
				t.Fatal(err)
			}
			q := db.New(reader)
			if kind == "notification" {
				source.Context, _ = json.Marshal(delegation.ResultTaskContext{Type: delegation.ResultTaskType})
				_, err = delegation.ManagedSourceTaskScope(ctx, q, source, actor.WorkspaceID)
			} else {
				_, err = resultScopeTaskID(ctx, q, source, address)
			}
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != "55P03" || IsDelegationResultRefusal(err) || errors.Is(err, delegation.ErrReturnInvalid) {
				t.Fatalf("temporary scope read became refusal or lost cause: %v", err)
			}
			if err := reader.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := locker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if kind == "notification" {
				_, err = delegation.ManagedSourceTaskScope(ctx, svc.Queries, source, actor.WorkspaceID)
				if !errors.Is(err, delegation.ErrReturnInvalid) {
					t.Fatalf("genuinely missing notification did not refuse: %v", err)
				}
				return
			}
			if id, err := resultScopeTaskID(ctx, svc.Queries, source, address); err != nil || id != util.UUIDToString(sourceID) {
				t.Fatalf("unlocked scope failed recovery: %q %v", id, err)
			}
			if _, err = pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id=$1`, sourceID); err != nil {
				t.Fatal(err)
			}
			if _, err = resultScopeTaskID(ctx, svc.Queries, source, address); !IsDelegationResultRefusal(err) {
				t.Fatalf("genuinely missing scope did not refuse: %v", err)
			}
		})
	}
}
