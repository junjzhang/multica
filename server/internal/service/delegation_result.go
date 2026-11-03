package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type DelegationResultStats struct{ Materialized, Refused int }
type resultRefusal string

func (e resultRefusal) Error() string { return string(e) }

func IsDelegationResultRefusal(err error) bool {
	var refusal resultRefusal
	return errors.As(err, &refusal) || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, delegation.ErrReturnInvalid)
}

// ReconcileDelegationResults recovers from committed terminal outcomes and
// accepted links. The event bus is only a latency optimization, never the outbox.
func (s *TaskService) ReconcileDelegationResults(ctx context.Context, limit int32) (DelegationResultStats, error) {
	var stats DelegationResultStats
	if limit <= 0 {
		return stats, nil
	}
	if _, err := s.Queries.PropagateRetryDelegationReturns(ctx, limit); err != nil {
		return stats, err
	}
	failures, err := s.Queries.ListUnresolvedDelegationFailures(ctx, limit)
	if err != nil {
		return stats, err
	}
	for _, failed := range failures {
		_, _, err := s.retryFailedTaskDecision(ctx, failed)
		if stampErr := s.Queries.RecordDelegationRetryDecisionAttempt(ctx, failed.ID); stampErr != nil {
			return stats, stampErr
		}
		if err != nil {
			slog.Warn("delegated failure decision pending", "task_id", util.UUIDToString(failed.ID), "error", err)
			continue
		}
	}
	ready, err := s.Queries.ListReadyDelegationResults(ctx, limit)
	if err != nil {
		return stats, err
	}
	for _, pair := range ready {
		created, refused, err := s.materializeDelegationResult(ctx, pair.TaskID, pair.DelegationReturnID)
		if err != nil || (!created && !refused) {
			if stampErr := s.Queries.RecordDelegationResultAttempt(ctx, db.RecordDelegationResultAttemptParams{TaskID: pair.TaskID, ReturnID: pair.DelegationReturnID}); stampErr != nil {
				return stats, stampErr
			}
		}
		if err != nil {
			slog.Warn("delegated result materialization pending", "task_id", util.UUIDToString(pair.TaskID), "return_id", util.UUIDToString(pair.DelegationReturnID), "error", err)
			continue
		}
		if created {
			stats.Materialized++
		}
		if refused {
			stats.Refused++
		}
	}
	return stats, nil
}

func (s *TaskService) materializeDelegationResult(ctx context.Context, taskID, returnID pgtype.UUID) (bool, bool, error) {
	address, err := s.Queries.GetDelegationReturnByID(ctx, returnID)
	if err != nil {
		return false, false, err
	}
	outcome, err := s.Queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return false, false, err
	}
	var source db.AgentTaskQueue
	var sourceErr error
	if address.ReturnKind == string(delegation.KindManagedTask) {
		source, sourceErr = s.Queries.GetAgentTask(ctx, address.SourceTaskID)
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	owners := []pgtype.UUID{address.IssueID}
	if source.IssueID.Valid {
		owners = append(owners, source.IssueID)
	}
	if _, err := q.LockDelegationResultIssues(ctx, db.LockDelegationResultIssuesParams{WorkspaceID: address.WorkspaceID, IssueIds: owners}); err != nil {
		return false, false, err
	}
	obligation, err := q.LockDelegationResultObligation(ctx, db.LockDelegationResultObligationParams{TaskID: taskID, DelegationReturnID: returnID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if obligation.NotificationTaskID.Valid || obligation.NotificationDeliveryID.Valid || obligation.NotificationError.Valid {
		return false, false, nil
	}
	outcome, err = q.GetAgentTask(ctx, taskID)
	if err != nil {
		return false, false, err
	}
	child, err := q.HasRetryTaskForParent(ctx, taskID)
	if err != nil {
		return false, false, err
	}
	if child || (outcome.Status != "completed" && !(outcome.Status == "failed" && outcome.DelegationOutcomeFinal)) {
		return false, false, nil
	}
	commitNotification := func() error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if s.Bus != nil {
			s.Bus.Publish(events.Event{
				Type:        protocol.EventIssueMailboxChanged,
				WorkspaceID: util.UUIDToString(address.WorkspaceID),
				ActorType:   "system",
				Payload:     map[string]any{"issue_id": util.UUIDToString(address.IssueID)},
			})
		}
		return nil
	}
	refuse := func(reason error) (bool, bool, error) {
		if err := q.FailDelegationResultNotification(ctx, db.FailDelegationResultNotificationParams{TaskID: taskID, DelegationReturnID: returnID, Reason: pgtype.Text{String: reason.Error(), Valid: true}}); err != nil {
			return false, false, err
		}
		return false, true, commitNotification()
	}
	if outcome.IssueID != address.IssueID {
		return refuse(resultRefusal("result_scope_mismatch"))
	}
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: address.IssueID, WorkspaceID: address.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(resultRefusal("delegated_scope_missing"))
	}
	if err != nil {
		return false, false, err
	}
	if issuestatus.Category(ctx, q, issue.WorkspaceID, issue.Status) == issuestatus.CategoryClosed {
		return refuse(resultRefusal("delegation_cancelled"))
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: address.DispatcherAgentID, WorkspaceID: address.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(resultRefusal("recipient_missing"))
	}
	if err != nil {
		return false, false, err
	}
	if agent.ArchivedAt.Valid {
		return refuse(resultRefusal("recipient_archived"))
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: agent.OwnerID, WorkspaceID: address.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return refuse(resultRefusal("recipient_owner_unavailable"))
	} else if err != nil {
		return false, false, err
	}
	var handling db.AgentTaskQueue
	if address.ReturnKind == string(delegation.KindExternalSession) {
		err = materializeExternalResult(ctx, q, outcome, address, agent)
	} else if sourceErr != nil {
		if !errors.Is(sourceErr, pgx.ErrNoRows) {
			return false, false, sourceErr
		}
		return refuse(resultRefusal("source_task_missing"))
	} else {
		handling, err = materializeManagedResult(ctx, q, outcome, address, agent, source)
	}
	var unavailable resultRefusal
	if errors.As(err, &unavailable) {
		return refuse(unavailable)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	} // existing queue slot; retain the same obligation
	if err != nil {
		return false, false, err
	}
	if err := commitNotification(); err != nil {
		return false, false, err
	}
	if handling.ID.Valid {
		s.NotifyTaskEnqueued(ctx, handling)
	}
	return true, false, nil
}

func materializeExternalResult(ctx context.Context, q *db.Queries, outcome db.AgentTaskQueue, address db.IssueDelegationReturn, agent db.Agent) error {
	if agent.ExecutionKind != "external" {
		return resultRefusal("recipient_kind_changed")
	}
	session, err := q.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{ID: address.ExternalSessionID, WorkspaceID: address.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return resultRefusal("external_session_missing")
	}
	if err != nil {
		return err
	}
	if session.AgentID != address.DispatcherAgentID || session.OwnerUserID != agent.OwnerID {
		return resultRefusal("external_session_owner_changed")
	}
	key := fmt.Sprintf("result:%s:return:%s", util.UUIDToString(outcome.ID), util.UUIDToString(address.ID))
	payload, err := json.Marshal(map[string]string{"result_task_id": util.UUIDToString(outcome.ID), "delegation_return_id": util.UUIDToString(address.ID), "issue_id": util.UUIDToString(address.IssueID), "outcome": outcome.Status})
	if err != nil {
		return err
	}
	notification, err := delivery.NewPostgres(q).Enqueue(ctx, delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: util.UUIDToString(address.WorkspaceID)},
		Source:          delivery.Source{Type: delegation.ResultTaskType, Key: key},
		Recipient:       delivery.Recipient{Type: "agent", ID: util.UUIDToString(address.DispatcherAgentID)},
		Target:          delivery.Target{SessionID: util.UUIDToString(address.ExternalSessionID)},
		NotificationKey: key, Kind: delegation.ResultTaskType, Payload: payload,
		Refs: delivery.Refs{IssueID: util.UUIDToString(address.IssueID)},
	})
	if err != nil {
		return err
	}
	deliveryID, err := util.ParseUUID(notification.ID)
	if err != nil {
		return err
	}
	return q.SetDelegationResultDelivery(ctx, db.SetDelegationResultDeliveryParams{TaskID: outcome.ID, DelegationReturnID: address.ID, DeliveryID: deliveryID})
}

func StrictResultProviderSupported(provider string) bool {
	switch provider {
	case "codex", "claude", "pi", "opencode", "dsh", "grok", "dim", "kiro", "qwenpaw", "traecli", "mcode", "kimi", "qoder", "zeroclaw", "reasonix", "hermes", "muse":
		return true
	default:
		return false
	}
}

func materializeManagedResult(ctx context.Context, q *db.Queries, outcome db.AgentTaskQueue, address db.IssueDelegationReturn, agent db.Agent, source db.AgentTaskQueue) (db.AgentTaskQueue, error) {
	var empty db.AgentTaskQueue
	if agent.ExecutionKind != "managed" || agent.RuntimeID != address.RuntimeID || source.AgentID != agent.ID {
		return empty, resultRefusal("managed_recipient_identity_changed")
	}
	runtime, err := q.LockRuntimeForDelegation(ctx, db.LockRuntimeForDelegationParams{ID: address.RuntimeID, WorkspaceID: address.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, resultRefusal("frozen_runtime_missing")
	}
	if err != nil {
		return empty, err
	}
	if runtime.Provider != address.Provider.String {
		return empty, resultRefusal("frozen_provider_changed")
	}
	if runtime.RuntimeMode != "local" || !StrictResultProviderSupported(runtime.Provider) {
		return empty, resultRefusal("unsupported_strict_result_runtime")
	}
	if runtime.Visibility == "private" && !RuntimeAllowsUser(runtime, agent.OwnerID) {
		return empty, resultRefusal("runtime_access_revoked")
	}
	if address.ScopeTaskID.Valid {
		source.IssueID, source.ChatSessionID, source.AutopilotRunID = pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}
	}
	if err := validateResultSourceScope(ctx, q, source, address.WorkspaceID); err != nil {
		return empty, err
	}
	scopeTaskID, err := resultScopeTaskID(ctx, q, source, address)
	if err != nil {
		return empty, err
	}
	meta := protocol.DelegationResultTurn{
		ResultTaskID: util.UUIDToString(outcome.ID), DelegationReturnID: util.UUIDToString(address.ID),
		SourceTaskID: util.UUIDToString(address.SourceTaskID), ScopeTaskID: scopeTaskID, DelegatedIssueID: util.UUIDToString(address.IssueID), Outcome: outcome.Status,
		RuntimeID: util.UUIDToString(address.RuntimeID), Provider: address.Provider.String, SessionID: address.SessionID.String, WorkDir: address.WorkDir.String,
	}
	contextJSON, err := json.Marshal(delegation.ResultTaskContext{Type: delegation.ResultTaskType, Result: meta})
	if err != nil {
		return empty, err
	}
	task, err := q.CreateDelegationResultTask(ctx, db.CreateDelegationResultTaskParams{
		ID: dbid.NewV7(), AgentID: agent.ID, RuntimeID: address.RuntimeID,
		IssueID: source.IssueID, ChatSessionID: source.ChatSessionID, AutopilotRunID: source.AutopilotRunID,
		Context: contextJSON, SessionID: address.SessionID, WorkDir: address.WorkDir,
		OriginatorUserID: source.OriginatorUserID, AccountableUserID: source.AccountableUserID,
		TriggerSummary: pgtype.Text{String: "Delegated outcome: " + meta.ResultTaskID, Valid: true},
	})
	if err != nil {
		return empty, err
	}
	if err := q.SetDelegationResultTask(ctx, db.SetDelegationResultTaskParams{TaskID: outcome.ID, DelegationReturnID: address.ID, NotificationTaskID: task.ID}); err != nil {
		return empty, err
	}
	return task, nil
}

func validateResultSourceScope(ctx context.Context, q *db.Queries, source db.AgentTaskQueue, workspace pgtype.UUID) error {
	var err error
	switch {
	case source.IssueID.Valid:
		_, err = q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: source.IssueID, WorkspaceID: workspace})
	case source.ChatSessionID.Valid:
		var chat db.ChatSession
		chat, err = q.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: source.ChatSessionID, WorkspaceID: workspace})
		if err == nil && chat.Status != "active" {
			return resultRefusal("source_chat_unavailable")
		}
	case source.AutopilotRunID.Valid:
		run, e := q.GetAutopilotRun(ctx, source.AutopilotRunID)
		if e != nil {
			err = e
			break
		}
		ap, e := q.GetAutopilot(ctx, run.AutopilotID)
		err = e
		if e == nil && ap.WorkspaceID != workspace {
			return resultRefusal("source_scope_foreign")
		}
	default:
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return resultRefusal("source_scope_missing")
	}
	return err
}

// VerifiedDelegationResult resolves the queue projection through its durable
// obligation. A context marker alone never grants access to a frozen address.
func VerifiedDelegationResult(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) (*protocol.DelegationResultTurn, error) {
	projected, parseErr := delegation.ResultTurn(task)
	link, err := q.GetDelegationResultObligationForTask(ctx, task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		if projected != nil || parseErr != nil {
			return nil, resultRefusal("unbound_result_task")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if parseErr != nil || projected == nil {
		return nil, resultRefusal("invalid_result_projection")
	}
	address, err := q.GetDelegationReturnByID(ctx, link.DelegationReturnID)
	if err != nil {
		return nil, err
	}
	outcome, err := q.GetAgentTask(ctx, link.TaskID)
	if err != nil {
		return nil, err
	}
	source, err := q.GetAgentTask(ctx, address.SourceTaskID)
	if err != nil {
		return nil, err
	}
	if address.ScopeTaskID.Valid {
		source.IssueID, source.ChatSessionID, source.AutopilotRunID = pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}
	}
	scopeTaskID, err := resultScopeTaskID(ctx, q, source, address)
	if err != nil {
		return nil, err
	}
	expected := protocol.DelegationResultTurn{
		ResultTaskID: util.UUIDToString(outcome.ID), DelegationReturnID: util.UUIDToString(address.ID),
		SourceTaskID: util.UUIDToString(source.ID), ScopeTaskID: scopeTaskID, DelegatedIssueID: util.UUIDToString(address.IssueID), Outcome: outcome.Status,
		RuntimeID: util.UUIDToString(address.RuntimeID), Provider: address.Provider.String,
		SessionID: address.SessionID.String, WorkDir: address.WorkDir.String,
	}
	if task.ForceFreshSession || *projected != expected || address.ReturnKind != string(delegation.KindManagedTask) ||
		task.AgentID != address.DispatcherAgentID || source.AgentID != task.AgentID ||
		task.RuntimeID != address.RuntimeID || task.IssueID != source.IssueID ||
		task.ChatSessionID != source.ChatSessionID || task.AutopilotRunID != source.AutopilotRunID ||
		task.SessionID != address.SessionID || task.WorkDir != address.WorkDir || outcome.IssueID != address.IssueID {
		return nil, resultRefusal("result_identity_mismatch")
	}
	if outcome.Status != "completed" && !(outcome.Status == "failed" && outcome.DelegationOutcomeFinal) {
		return nil, resultRefusal("result_not_final")
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: task.AgentID, WorkspaceID: address.WorkspaceID})
	if err != nil {
		return nil, err
	}
	if agent.ExecutionKind != "managed" || agent.ArchivedAt.Valid || agent.RuntimeID != address.RuntimeID {
		return nil, resultRefusal("recipient_unavailable")
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: agent.OwnerID, WorkspaceID: address.WorkspaceID}); err != nil {
		return nil, err
	}
	runtime, err := q.GetAgentRuntime(ctx, address.RuntimeID)
	if err != nil {
		return nil, err
	}
	if runtime.WorkspaceID != address.WorkspaceID || runtime.Provider != expected.Provider || runtime.RuntimeMode != "local" ||
		!StrictResultProviderSupported(runtime.Provider) || (runtime.Visibility == "private" && !RuntimeAllowsUser(runtime, agent.OwnerID)) {
		return nil, resultRefusal("frozen_runtime_unavailable")
	}
	if address.ScopeTaskID.Valid {
		source.IssueID, source.ChatSessionID, source.AutopilotRunID = pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}
	}
	if err := validateResultSourceScope(ctx, q, source, address.WorkspaceID); err != nil {
		return nil, err
	}
	return &expected, nil
}

func resultScopeTaskID(ctx context.Context, q *db.Queries, source db.AgentTaskQueue, address db.IssueDelegationReturn) (string, error) {
	if source.IssueID.Valid || source.ChatSessionID.Valid || source.AutopilotRunID.Valid {
		if address.ScopeTaskID.Valid {
			return "", resultRefusal("result_scope_mismatch")
		}
		return "", nil
	}
	if err := delegation.ValidateOriginalTaskScope(ctx, q, address); err != nil {
		if errors.Is(err, delegation.ErrReturnInvalid) {
			return "", resultRefusal("original_task_scope_unavailable")
		}
		return "", err
	}
	return util.UUIDToString(address.ScopeTaskID), nil
}
