package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/delegation"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// acceptedCommentObligation is one durable target a comment was accepted for.
// It is recorded in the comment's transaction; the trigger is carried here only
// so the same transaction can try to cover it immediately.
type acceptedCommentObligation struct {
	ID      pgtype.UUID
	Trigger commentAgentTrigger
}

// commentObligationCoverage is the outcome of trying to cover one accepted
// comment target. settled means a run (new, folded, or active with this comment
// durably planned) now owns the comment; an unsettled result leaves the
// obligation pending for the background consumer.
type commentObligationCoverage struct {
	status      DispatchStatus
	reason      DispatchReasonCode
	settled     bool
	settledTask pgtype.UUID
	taskCreated bool
	task        db.AgentTaskQueue
	attr        attribution.Result
	mergedAttr  attribution.Result
	merged      bool
}

// recordAcceptedCommentObligations writes one row per accepted comment target
// through the comment's own transaction. The row stores the exact executing
// agent, optional squad, trigger source and accepted return; replay never
// re-derives them from the comment text or the issue's current assignee.
func (h *Handler) recordAcceptedCommentObligations(ctx context.Context, q *db.Queries, issue db.Issue, commentID pgtype.UUID, triggers []commentAgentTrigger, returnID pgtype.UUID) ([]acceptedCommentObligation, error) {
	accepted := make([]acceptedCommentObligation, 0, len(triggers))
	for _, trigger := range triggers {
		var squadID pgtype.UUID
		if trigger.Squad != nil {
			squadID = trigger.Squad.ID
		}
		params := db.InsertCommentDispatchObligationParams{
			ID:                 dbid.NewV7(),
			WorkspaceID:        issue.WorkspaceID,
			IssueID:            issue.ID,
			CommentID:          commentID,
			AgentID:            trigger.Agent.ID,
			SquadID:            squadID,
			TriggerSource:      string(trigger.Source),
			DelegationReturnID: returnID,
		}
		row, err := q.InsertCommentDispatchObligation(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("record comment dispatch obligation: %w", err)
		}
		accepted = append(accepted, acceptedCommentObligation{ID: row.ID, Trigger: trigger})
	}
	return accepted, nil
}

// coverAcceptedCommentObligationsInTx is the one normal path for an
// agent-authored accepted comment. It tries, in order, to fold the comment
// into a pre-claim run, register it on a claim-receipt run, or create a fresh
// run. Every attempt is a savepoint so a transient failure leaves the comment
// transaction healthy; a target that still cannot be covered stays pending for
// the sweeper instead of being lost after commit.
func (h *Handler) coverAcceptedCommentObligationsInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, commentID pgtype.UUID, obligations []acceptedCommentObligation, returnIDs []pgtype.UUID) (map[string]commentEnqueueResult, []commentTriggerTask, []attribution.Result, error) {
	results := make(map[string]commentEnqueueResult, len(obligations))
	var tasks []commentTriggerTask
	var attrs []attribution.Result
	for _, obligation := range obligations {
		coverage := h.coverCommentObligationInTx(ctx, tx, q, issue, commentID, obligation, returnIDs)
		execSquadID := ""
		if obligation.Trigger.Squad != nil {
			execSquadID = uuidToString(obligation.Trigger.Squad.ID)
		}
		results[uuidToString(obligation.Trigger.Agent.ID)] = commentEnqueueResult{
			status:      coverage.status,
			reason:      coverage.reason,
			execSquadID: execSquadID,
		}
		if coverage.taskCreated {
			tasks = append(tasks, commentTriggerTask{task: coverage.task, attr: coverage.attr})
		}
		if coverage.merged {
			attrs = append(attrs, coverage.mergedAttr)
		}
		if !coverage.settled {
			continue
		}
		if err := q.SettleCommentDispatchObligation(ctx, db.SettleCommentDispatchObligationParams{
			TaskID: coverage.settledTask,
			ID:     obligation.ID,
		}); err != nil {
			return nil, nil, nil, fmt.Errorf("settle comment dispatch obligation: %w", err)
		}
	}
	return results, tasks, attrs, nil
}

// coverCommentObligationInTx uses the same intent -> run rule as replay. A
// queued sibling is folded atomically; a claimed sibling receives a planned
// comment; otherwise a fresh run is created. A duplicate-key loser re-observes
// the occupant's current state and retries, so a sibling that completed or
// started between two statements cannot produce a silent no-op.
func (h *Handler) coverCommentObligationInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, commentID pgtype.UUID, obligation acceptedCommentObligation, returnIDs []pgtype.UUID) commentObligationCoverage {
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if coverage, settle := h.foldCommentObligationInTx(ctx, tx, issue, commentID, obligation); settle {
			return coverage
		}
		if coverage, settle := h.planCommentObligationInTx(ctx, tx, q, issue, commentID, obligation); settle {
			return coverage
		}
		coverage, retry := h.enqueueCommentObligationInTx(ctx, tx, q, issue, commentID, obligation, returnIDs)
		if coverage.settled {
			return coverage
		}
		if !retry {
			return coverage
		}
	}
	slog.Warn("comment dispatch obligation not covered yet; leaving it for the recovery consumer",
		"issue_id", uuidToString(issue.ID),
		"comment_id", uuidToString(commentID),
		"agent_id", uuidToString(obligation.Trigger.Agent.ID))
	return commentObligationCoverage{status: DispatchDeferred, reason: ReasonDeferred}
}

func (h *Handler) foldCommentObligationInTx(ctx context.Context, tx pgx.Tx, issue db.Issue, commentID pgtype.UUID, obligation acceptedCommentObligation) (commentObligationCoverage, bool) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return commentObligationCoverage{}, false
	}
	qsp := h.Queries.WithTx(sp)
	params, attr, mergeResult := h.mergeCommentIntoPendingTaskParams(ctx, qsp, issue, obligation.Trigger, commentID, h.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID))
	if mergeResult != commentMergeSucceeded {
		_ = sp.Rollback(ctx)
		if mergeResult == commentMergeAttributionBlocked {
			return commentObligationCoverage{status: DispatchBlocked, reason: ReasonAttributionBlocked}, true
		}
		return commentObligationCoverage{}, false
	}
	row, err := h.mergeCommentIntoPendingTaskWithQueries(ctx, qsp, params, commentID)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isNotFound(err) {
			return commentObligationCoverage{}, false
		}
		slog.Warn("fold accepted comment obligation failed",
			"issue_id", uuidToString(issue.ID),
			"comment_id", uuidToString(commentID),
			"agent_id", uuidToString(obligation.Trigger.Agent.ID),
			"error", err)
		return commentObligationCoverage{}, false
	}
	if err := sp.Commit(ctx); err != nil {
		return commentObligationCoverage{}, false
	}
	return commentObligationCoverage{
		status:      DispatchCoalesced,
		reason:      ReasonCoalesced,
		settled:     true,
		settledTask: row.ID,
		merged:      true,
		mergedAttr:  attr,
	}, true
}

func (h *Handler) planCommentObligationInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, commentID pgtype.UUID, obligation acceptedCommentObligation) (commentObligationCoverage, bool) {
	active, activeErr := q.GetNewestActiveTaskForIssueAgentThread(ctx, db.GetNewestActiveTaskForIssueAgentThreadParams{
		IssueID:   issue.ID,
		AgentID:   obligation.Trigger.Agent.ID,
		CommentID: commentID,
	})
	if activeErr != nil {
		if !isNotFound(activeErr) {
			slog.Warn("read active task for comment obligation failed",
				"issue_id", uuidToString(issue.ID),
				"comment_id", uuidToString(commentID),
				"agent_id", uuidToString(obligation.Trigger.Agent.ID),
				"error", activeErr)
		}
		return commentObligationCoverage{}, false
	}
	comment, commentErr := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
		ID:          commentID,
		WorkspaceID: issue.WorkspaceID,
	})
	if commentErr != nil {
		return commentObligationCoverage{}, false
	}
	// A comment newer than the active run is already visible to that run's
	// completion reconcile by timestamp. Registering it as a planned id as
	// well would give the same comment two replay paths. Only a comment that
	// PREDATES the run (the lost enqueue race) needs the planned-id hand-off.
	if !active.CreatedAt.Valid || !comment.CreatedAt.Valid || !active.CreatedAt.Time.After(comment.CreatedAt.Time) {
		return commentObligationCoverage{}, false
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return commentObligationCoverage{}, false
	}
	qsp := h.Queries.WithTx(sp)
	row, err := qsp.RegisterPlannedCommentForActiveTask(ctx, db.RegisterPlannedCommentForActiveTaskParams{
		CommentID: commentID,
		IssueID:   issue.ID,
		AgentID:   obligation.Trigger.Agent.ID,
		HeadSha:   h.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID),
	})
	if err != nil {
		_ = sp.Rollback(ctx)
		if !isNotFound(err) {
			slog.Warn("register accepted comment obligation on active task failed",
				"issue_id", uuidToString(issue.ID),
				"comment_id", uuidToString(commentID),
				"agent_id", uuidToString(obligation.Trigger.Agent.ID),
				"error", err)
		}
		return commentObligationCoverage{}, false
	}
	if err := linkCommentRunReturns(ctx, qsp, row.ID, commentID); err != nil {
		_ = sp.Rollback(ctx)
		return commentObligationCoverage{}, false
	}
	if err := sp.Commit(ctx); err != nil {
		return commentObligationCoverage{}, false
	}
	return commentObligationCoverage{
		status:      DispatchDeferred,
		reason:      ReasonDeferred,
		settled:     true,
		settledTask: row.ID,
	}, true
}

func (h *Handler) enqueueCommentObligationInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, commentID pgtype.UUID, obligation acceptedCommentObligation, returnIDs []pgtype.UUID) (commentObligationCoverage, bool) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return commentObligationCoverage{}, false
	}
	qsp := h.Queries.WithTx(sp)
	task, attr, enqueueErr := h.enqueueSingleCommentTriggerInTx(ctx, qsp, issue, commentID, obligation.Trigger, returnIDs)
	if enqueueErr != nil {
		_ = sp.Rollback(ctx)
		if errors.Is(enqueueErr, service.ErrDuplicatePendingTask) {
			return commentObligationCoverage{}, true
		}
		slog.Warn("enqueue accepted comment obligation failed",
			"issue_id", uuidToString(issue.ID),
			"comment_id", uuidToString(commentID),
			"agent_id", uuidToString(obligation.Trigger.Agent.ID),
			"error", enqueueErr)
		return commentObligationCoverage{status: DispatchBlocked, reason: commentEnqueueFailureReason(enqueueErr)}, false
	}
	if err := sp.Commit(ctx); err != nil {
		return commentObligationCoverage{}, false
	}
	return commentObligationCoverage{
		status:      DispatchQueued,
		reason:      ReasonQueued,
		settled:     true,
		settledTask: task.ID,
		taskCreated: true,
		task:        task,
		attr:        attr,
	}, true
}

// CommentDispatchObligationStats reports one recovery tick.
type CommentDispatchObligationStats struct {
	Scanned int
	Covered int
	Failed  int
}

// ConsumeCommentDispatchObligations drains pending accepted-comment
// obligations. It is called by the server's periodic sweeper, and by tests
// that simulate a crash/restart: the durable row, not an in-request callback,
// is what makes progress.
func (h *Handler) ConsumeCommentDispatchObligations(ctx context.Context, batchSize int32) (CommentDispatchObligationStats, error) {
	var stats CommentDispatchObligationStats
	if batchSize <= 0 {
		return stats, nil
	}
	pending, err := h.Queries.ListPendingCommentDispatchObligations(ctx, batchSize)
	if err != nil {
		return stats, fmt.Errorf("list comment dispatch obligations: %w", err)
	}
	stats.Scanned = len(pending)
	for _, candidate := range pending {
		covered, failed, err := h.consumeCommentDispatchObligation(ctx, candidate.ID)
		if err != nil {
			slog.Warn("comment dispatch obligation consumer failed",
				"obligation_id", uuidToString(candidate.ID),
				"error", err)
			continue
		}
		if covered {
			stats.Covered++
		}
		if failed {
			stats.Failed++
		}
	}
	return stats, nil
}

func (h *Handler) consumeCommentDispatchObligation(ctx context.Context, obligationID pgtype.UUID) (bool, bool, error) {
	coverage, failed, err := h.dispatchCommentObligation(ctx, obligationID, false)
	return coverage.settled, failed, err
}

// Replay and the sweeper serialize against edits and each other. The exact
// obligation identifies the intent generation; a prior edit's run is not proof
// that the current obligation was delivered.
func (h *Handler) replayCommentDispatchObligation(ctx context.Context, commentID, agentID pgtype.UUID) (commentObligationCoverage, bool) {
	obligation, err := h.Queries.GetCommentDispatchObligationForTarget(ctx, db.GetCommentDispatchObligationForTargetParams{
		CommentID: commentID, AgentID: agentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return commentObligationCoverage{}, false
	}
	if err != nil {
		return commentObligationCoverage{status: DispatchBlocked, reason: ReasonInternalError}, true
	}
	coverage, _, err := h.dispatchCommentObligation(ctx, obligation.ID, true)
	if err != nil {
		slog.Warn("replay comment obligation failed", "obligation_id", uuidToString(obligation.ID), "error", err)
		return commentObligationCoverage{status: DispatchBlocked, reason: ReasonInternalError}, true
	}
	return coverage, true
}

func commentChangeCancelledTask(task db.AgentTaskQueue) bool {
	var cancellation struct {
		TaskID string `json:"comment_change_cancelled_task_id"`
	}
	_ = json.Unmarshal(task.Context, &cancellation)
	return task.Status == "cancelled" && cancellation.TaskID == uuidToString(task.ID)
}

func (h *Handler) dispatchCommentObligation(ctx context.Context, obligationID pgtype.UUID, replay bool) (commentObligationCoverage, bool, error) {
	var empty commentObligationCoverage
	candidate, err := h.Queries.GetCommentDispatchObligation(ctx, obligationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	comment, commentErr := q.LockLiveComment(ctx, db.LockLiveCommentParams{ID: candidate.CommentID, WorkspaceID: candidate.WorkspaceID})
	if commentErr != nil && !errors.Is(commentErr, pgx.ErrNoRows) {
		return empty, false, commentErr
	}
	obligation, err := q.LockCommentDispatchObligation(ctx, obligationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	if obligation.Status == "failed" || (!replay && obligation.Status != "pending") {
		return commentObligationCoverage{status: DispatchBlocked, reason: ReasonTargetUnavailable}, false, nil
	}
	fail := func(reason DispatchReasonCode, detail string) (commentObligationCoverage, bool, error) {
		if err := q.FailCommentDispatchObligation(ctx, db.FailCommentDispatchObligationParams{
			LastError: pgtype.Text{String: string(reason) + ": " + detail, Valid: true}, ID: obligation.ID,
		}); err != nil {
			return empty, false, err
		}
		return commentObligationCoverage{status: DispatchBlocked, reason: reason}, true, tx.Commit(ctx)
	}
	if commentErr != nil {
		return fail(ReasonTargetUnavailable, "comment no longer exists")
	}
	issue, err := q.GetIssue(ctx, obligation.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ReasonTargetUnavailable, "issue no longer exists")
	}
	if err != nil {
		return empty, false, err
	}
	var returnIDs []pgtype.UUID
	if obligation.DelegationReturnID.Valid {
		returnIDs = []pgtype.UUID{obligation.DelegationReturnID}
	}
	// Only this generation's covering run can prove delivery. In particular,
	// a completed run with the same comment id from before an edit cannot.
	if obligation.SettledTaskID.Valid {
		task, err := q.GetAgentTask(ctx, obligation.SettledTaskID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, false, err
		}
		// Delivery proves input admission, not completion of a batch cancelled
		// by an edit/delete. Its surviving intents still require execution.
		if !commentChangeCancelledTask(task) && slices.Contains(task.DeliveredCommentIds, comment.ID) {
			if err := delegation.LinkRun(ctx, q, task.ID, returnIDs...); err != nil {
				return empty, false, err
			}
			if err := q.SettleCommentDispatchObligation(ctx, db.SettleCommentDispatchObligationParams{ID: obligation.ID, TaskID: task.ID}); err != nil {
				return empty, false, err
			}
			return commentObligationCoverage{status: DispatchCoalesced, reason: ReasonCoalesced, settled: true, settledTask: task.ID}, false, tx.Commit(ctx)
		}
	}
	if err := q.ReopenCommentDispatchObligation(ctx, obligation.ID); err != nil {
		return empty, false, err
	}
	if comment.AuthorType == "agent" {
		author, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: comment.AuthorID, WorkspaceID: obligation.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && author.ArchivedAt.Valid) {
			return fail(ReasonInvocationNotAllowed, "comment author no longer available")
		}
		if err != nil {
			return empty, false, err
		}
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: obligation.AgentID, WorkspaceID: obligation.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ReasonTargetUnavailable, "accepted target agent no longer exists")
	}
	if err != nil {
		return empty, false, err
	}
	originator := uuidToString(comment.AuthorID)
	if comment.AuthorType != "member" {
		originator = uuidToString(h.TaskService.ResolveOriginatorFromTriggerComment(ctx, issue.WorkspaceID, comment.ID))
	}
	if !h.canInvokeAgent(ctx, agent, comment.AuthorType, uuidToString(comment.AuthorID), originator, uuidToString(issue.WorkspaceID)) {
		return fail(ReasonInvocationNotAllowed, "accepted target is no longer invocable")
	}
	if agent.ArchivedAt.Valid || agent.ExecutionKind == "external" {
		return fail(ReasonTargetUnavailable, "accepted target is no longer executable")
	}
	trigger := commentAgentTrigger{Agent: agent, Source: commentAgentTriggerSource(obligation.TriggerSource)}
	if obligation.SquadID.Valid {
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: obligation.SquadID, WorkspaceID: obligation.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && squad.LeaderID != agent.ID) {
			return fail(ReasonTargetUnavailable, "accepted squad leader no longer available")
		}
		if err != nil {
			return empty, false, err
		}
		trigger.Squad = &squad
	}
	verdict, err := service.AgentReadiness(ctx, h.runtimeLookup(obsmetrics.RuntimeLookupSourceComment), agent)
	if err != nil {
		_, _, retryErr := h.recordCommentObligationRetry(ctx, tx, q, obligation.ID, err)
		return empty, false, retryErr
	}
	if verdict.Blocked() {
		return fail(verdict.Reason, verdict.Detail)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	qsp := h.Queries.WithTx(sp)
	coverage := h.coverCommentObligationInTx(ctx, sp, qsp, issue, obligation.CommentID, acceptedCommentObligation{ID: obligation.ID, Trigger: trigger}, returnIDs)
	if coverage.settled {
		err = qsp.SettleCommentDispatchObligation(ctx, db.SettleCommentDispatchObligationParams{TaskID: coverage.settledTask, ID: obligation.ID})
	}
	if err != nil || !coverage.settled {
		_ = sp.Rollback(ctx)
		cause := err
		if cause == nil {
			cause = fmt.Errorf("no covering run yet")
		}
		_, _, retryErr := h.recordCommentObligationRetry(ctx, tx, q, obligation.ID, cause)
		return commentObligationCoverage{status: DispatchBlocked, reason: ReasonInternalError}, false, retryErr
	}
	if err := sp.Commit(ctx); err != nil {
		return empty, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, false, err
	}
	if coverage.merged {
		h.TaskService.RecordHumanDelegation(ctx, coverage.mergedAttr)
	}
	if coverage.taskCreated {
		h.TaskService.ApplyQueuedIssueTaskOverlay(ctx, coverage.task)
		h.TaskService.PublishQueuedIssueTask(ctx, coverage.task, coverage.attr)
	}
	return coverage, false, nil
}

func (h *Handler) recordCommentObligationRetry(ctx context.Context, tx pgx.Tx, q *db.Queries, obligationID pgtype.UUID, cause error) (bool, bool, error) {
	row, err := q.GetCommentDispatchObligation(ctx, obligationID)
	if err != nil {
		return false, false, err
	}
	backoff := time.Duration(1<<minInt(row.Attempts, 10)) * time.Minute
	if backoff > time.Hour {
		backoff = time.Hour
	}
	if err := q.RecordCommentDispatchObligationAttempt(ctx, db.RecordCommentDispatchObligationAttemptParams{
		LastError:     pgtype.Text{String: cause.Error(), Valid: true},
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(backoff), Valid: true},
		ID:            obligationID,
	}); err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return false, false, nil
}

func minInt(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
