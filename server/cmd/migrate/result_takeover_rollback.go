package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

var resultTakeoverMigrationVersions = []string{
	"639_result_takeover",
	"640_current_delegation_return_index",
	"641_retire_delegation_return_dispatch_index",
	"642_result_takeover_request_index",
	"643_result_takeover_selection_version",
}

// Protect every rollback entry point, including partially applied upgrades.
// The first guard runs before 643 can remove a saved selection proof. Writers
// must be stopped for the entire sequence; the migration lock does not fence
// application transactions across the concurrent index steps.
func ensureResultTakeoverRollbackSafe(ctx context.Context, pool *pgxpool.Pool) error {
	var incompatible bool
	err := pool.QueryRow(ctx, `
 SELECT EXISTS (SELECT 1 FROM delegation_result_takeover)
 OR EXISTS (SELECT 1 FROM agent_task_delegation_return WHERE takeover_id IS NOT NULL)
 OR EXISTS (SELECT 1 FROM issue_delegation_return WHERE is_current IS DISTINCT FROM true)
 OR EXISTS (
  SELECT 1 FROM issue_delegation_return
  GROUP BY workspace_id,issue_id,dispatcher_agent_id HAVING count(*)>1
 )`).Scan(&incompatible)
	if err != nil {
		return fmt.Errorf("inspect result-takeover rollback ownership: %w", err)
	}
	if incompatible {
		return errors.New("cannot roll back result takeover while accepted history or incompatible return relationships exist; retain the takeover-capable schema and application, do not delete history to force rollback")
	}
	return nil
}
