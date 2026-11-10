package service

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func terminalResourceTask(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

var ErrDelegationResourcesRetained = errors.New("delegation resources are still retained")

// RequireDelegationScopeReleased runs after the scope's delete lock. Deletion
// must not erase the task that proves where an accepted result belongs.
func (s *TaskService) RequireDelegationScopeReleased(ctx context.Context, q *db.Queries, workspace, issue, chat pgtype.UUID) error {
	return requireManagedReturnPages(ctx, q, func(after pgtype.UUID) ([]db.IssueDelegationReturn, error) {
		return q.ListManagedReturnsForScope(ctx, db.ListManagedReturnsForScopeParams{WorkspaceID: workspace, IssueID: issue, ChatSessionID: chat, AfterReturnID: after})
	})
}

func RequireRuntimeDelegationResourcesReleased(ctx context.Context, q *db.Queries, runtime pgtype.UUID) error {
	return requireManagedReturnPages(ctx, q, func(after pgtype.UUID) ([]db.IssueDelegationReturn, error) {
		return q.ListManagedReturnsForRuntime(ctx, db.ListManagedReturnsForRuntimeParams{RuntimeID: runtime, AfterReturnID: after})
	})
}

func RequireWorkspaceDelegationResourcesReleased(ctx context.Context, q *db.Queries, workspace pgtype.UUID) error {
	return requireManagedReturnPages(ctx, q, func(after pgtype.UUID) ([]db.IssueDelegationReturn, error) {
		return q.ListManagedReturnsForWorkspace(ctx, db.ListManagedReturnsForWorkspaceParams{WorkspaceID: workspace, AfterReturnID: after})
	})
}

func requireManagedReturnPages(ctx context.Context, q *db.Queries, load func(pgtype.UUID) ([]db.IssueDelegationReturn, error)) error {
	var after pgtype.UUID
	for {
		refs, err := load(after)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return nil
		}
		if err := requireManagedReturnsReleased(ctx, q, refs); err != nil {
			return err
		}
		after = refs[len(refs)-1].ID
	}
}

func requireManagedReturnsReleased(ctx context.Context, q *db.Queries, refs []db.IssueDelegationReturn) error {
	for _, ref := range refs {
		sources := []pgtype.UUID{ref.SourceTaskID}
		if ref.ScopeTaskID.Valid && ref.ScopeTaskID != ref.SourceTaskID {
			sources = append(sources, ref.ScopeTaskID)
		}
		for _, id := range sources {
			state, err := q.GetResourceTaskState(ctx, db.GetResourceTaskStateParams{TaskID: id, WorkspaceID: ref.WorkspaceID})
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrDelegationResourcesRetained
			}
			if err != nil {
				return err
			}
			if state.AgentID != ref.DispatcherAgentID || !terminalResourceTask(state.Status) || (state.ResourceControlRequired && !state.TerminalReportedAt.Valid) {
				return ErrDelegationResourcesRetained
			}
		}
		reason, err := delegationResourceRetention(ctx, q, ref)
		if err != nil {
			return err
		}
		if reason != "" {
			return ErrDelegationResourcesRetained
		}
	}
	return nil
}

func delegationResourceRetention(ctx context.Context, q *db.Queries, ref db.IssueDelegationReturn) (string, error) {
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: ref.IssueID, WorkspaceID: ref.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "delegated_scope_unknown", nil
	}
	if err != nil {
		return "", err
	}
	category := issuestatus.Category(ctx, q, ref.WorkspaceID, issue.Status)
	if category != issuestatus.CategoryDone && category != issuestatus.CategoryClosed {
		return "delegation_not_accepted", nil
	}
	pending, err := q.HasPendingReturnInputs(ctx, ref.ID)
	if err != nil {
		return "", err
	}
	if pending {
		return "delegated_input_pending", nil
	}
	deps, err := q.ListReturnResultDependencies(ctx, ref.ID)
	if err != nil {
		return "", err
	}
	for _, dep := range deps {
		reason, err := resultDependencyRetention(ctx, q, dep)
		if err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

func resultDependencyRetention(ctx context.Context, q *db.Queries, dep db.ListReturnResultDependenciesRow) (string, error) {
	if !dep.ExecutionStatus.Valid || !terminalResourceTask(dep.ExecutionStatus.String) {
		return "delegated_execution_active", nil
	}
	if dep.ResourceControlRequired.Bool && !dep.TerminalReportedAt.Valid {
		return "delegated_execution_exit_unknown", nil
	}
	link := dep.AgentTaskDelegationReturn
	if !link.NotificationTaskID.Valid {
		if link.NotificationError.Valid || dep.ExecutionStatus.String == "cancelled" {
			return "", nil
		}
		retried, err := q.HasRetryTaskForParent(ctx, link.TaskID)
		if err != nil {
			return "", err
		}
		if retried {
			return "", nil
		}
		return "result_delivery_pending", nil
	}
	if !dep.HandlingStatus.Valid || !terminalResourceTask(dep.HandlingStatus.String) {
		return "result_delivery_pending", nil
	}
	if dep.HandlingResourceControlRequired.Bool && !dep.HandlingTerminalReportedAt.Valid {
		return "result_execution_exit_unknown", nil
	}
	if dep.MailboxProcessingStatus.String == "handled" {
		return "", nil
	}
	if dep.HandlingStatus.String == "failed" && dep.HandlingFailureReason.String != protocol.DelegationResultUnavailable {
		return "result_delivery_unknown", nil
	}
	if dep.HandlingStatus.String == "cancelled" && dep.HandlingStartedAt.Valid {
		return "result_delivery_unknown", nil
	}
	if dep.HandlingStatus.String == "completed" {
		return "result_consumption_pending", nil
	}
	return "", nil
}
