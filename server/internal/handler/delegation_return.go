package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ReturnSessionHeader lets an external agent name the host session that must
// receive its delegation results. The CLI fills it from --return-session or
// MULTICA_RETURN_SESSION (and, outside a managed task, the provider's own
// current session id). The server never trusts the value by itself: it must
// resolve to exactly one session registered to the authenticated agent in this
// workspace.
const ReturnSessionHeader = "X-Return-Session"

// delegationReturnRequired reports whether this dispatch needs a return address:
// an agent is handing executable work to an agent or squad. Human creates, issues
// with no executor, pure status/description writes, and discussions without an
// executable mention stay on their existing path.
func delegationReturnRequired(actorType string, assigneeType pgtype.Text) bool {
	if actorType != "agent" || !assigneeType.Valid {
		return false
	}
	return assigneeType.String == "agent" || assigneeType.String == "squad"
}

// resolveDelegationAddress derives the acting agent's return address from
// server-stamped request context. A managed run is addressed by the task id the
// auth middleware took from the mat_ token; an external session by its explicit
// host-session reference. Anything else on an agent request fails closed.
func (h *Handler) resolveDelegationAddress(r *http.Request, workspaceID, actorType, actorID string) (delegation.Address, error) {
	if actorType != "agent" {
		return delegation.Address{}, delegation.ErrReturnMissing
	}
	switch r.Header.Get("X-Actor-Source") {
	case "task_token":
		return delegation.ResolveManaged(r.Context(), h.Queries, workspaceID, actorID, r.Header.Get("X-Task-ID"))
	case "external_agent":
		return delegation.ResolveExternal(r.Context(), h.Queries, workspaceID, actorID, r.Header.Get(ReturnSessionHeader))
	default:
		// Legacy agent headers (X-Agent-ID + X-Task-ID without the stamped
		// source) are still accepted by resolveActor inside tests and by the
		// oldest daemons. Prefer the task if it is present; otherwise there is
		// no evidence of an address.
		if taskID := strings.TrimSpace(r.Header.Get("X-Task-ID")); taskID != "" {
			return delegation.ResolveManaged(r.Context(), h.Queries, workspaceID, actorID, taskID)
		}
		return delegation.Address{}, fmt.Errorf("%w: this request did not come from a task token or an external session", delegation.ErrReturnMissing)
	}
}

// acceptedDelegationReturn returns the issue's already accepted recipient. The
// first accepted delegation owns it; later assignments, reruns, and folded
// comments reuse it rather than replacing someone else's result recipient.
func (h *Handler) acceptedDelegationReturn(ctx context.Context, issue db.Issue) (db.IssueDelegationReturn, bool) {
	accepted, err := h.Queries.GetIssueDelegationReturn(ctx, db.GetIssueDelegationReturnParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			// A read failure is not evidence that the issue has no accepted
			// recipient; callers that need a hard gate treat it as missing.
			return db.IssueDelegationReturn{}, false
		}
		return db.IssueDelegationReturn{}, false
	}
	return accepted, true
}

// ensureDelegationReturnForDispatch validates and records the actor's accepted
// return for an existing issue before a run is queued. An existing accepted
// return is returned untouched. A member actor needs no return.
func (h *Handler) ensureDelegationReturnForDispatch(r *http.Request, issue db.Issue, actorType, actorID string) (db.IssueDelegationReturn, bool, error) {
	if accepted, ok := h.acceptedDelegationReturn(r.Context(), issue); ok {
		return accepted, true, nil
	}
	if actorType != "agent" {
		return db.IssueDelegationReturn{}, false, nil
	}
	address, err := h.resolveDelegationAddress(r, util.UUIDToString(issue.WorkspaceID), actorType, actorID)
	if err != nil {
		return db.IssueDelegationReturn{}, true, err
	}
	accepted, err := delegation.Ensure(r.Context(), h.Queries, util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), address, requestUserID(r))
	if err != nil {
		return db.IssueDelegationReturn{}, true, err
	}
	return accepted, true, nil
}

// linkTaskDelegationReturn attaches an already-queued run to the accepted
// return. The issue-level row remains authoritative, so a failed link is logged
// instead of discarding a run that is already durable.
func (h *Handler) linkTaskDelegationReturn(ctx context.Context, taskID, acceptedID pgtype.UUID) {
	if !taskID.Valid || !acceptedID.Valid {
		return
	}
	if _, err := h.Queries.SetAgentTaskDelegationReturn(ctx, db.SetAgentTaskDelegationReturnParams{
		ID:                 taskID,
		DelegationReturnID: acceptedID,
	}); err != nil {
		slog.Warn("link task to accepted delegation return failed",
			"task_id", util.UUIDToString(taskID),
			"delegation_return_id", util.UUIDToString(acceptedID),
			"error", err,
		)
	}
}

// writeDelegationReturnError reports a refused delegation with a stable code so
// an agent can distinguish "retry after the session pin lands" from "your
// address is wrong".
func writeDelegationReturnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, delegation.ErrReturnNotReady):
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":  "delegation_return_not_ready",
			"error": "The delegating run has not recorded its provider session yet. Retry the delegation after this run's session is persisted; no child work was created.",
		})
	case errors.Is(err, delegation.ErrReturnInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":  "delegation_return_invalid",
			"error": err.Error(),
		})
	case errors.Is(err, delegation.ErrReturnMissing):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":  "delegation_return_missing",
			"error": err.Error(),
		})
	default:
		writeError(w, http.StatusInternalServerError, "failed to resolve the delegation return address")
	}
}

// prepareCommentDelegationReturn validates the author's return address when a
// comment starts managed runs (an explicit @mention resolved to a native task).
// The comment is written only after this succeeds, so a missing or invalid
// address cannot leave an executable half-delegation behind. Discussions with
// no managed target, and ordinary member comments, are untouched.
func (h *Handler) prepareCommentDelegationReturn(r *http.Request, issue db.Issue, actorType, actorID string, plan commentTriggerPlan) (db.IssueDelegationReturn, error) {
	if len(plan.nativeTargets) == 0 || actorType != "agent" {
		return db.IssueDelegationReturn{}, nil
	}
	accepted, _, err := h.ensureDelegationReturnForDispatch(r, issue, actorType, actorID)
	return accepted, err
}

// linkCommentTriggeredTasks attaches the runs this exact comment started to the
// accepted return. A comment folded into an existing run keeps that run's
// original recipient; completion replay has no request context and is left on
// the issue's accepted return.
func (h *Handler) linkCommentTriggeredTasks(ctx context.Context, issueID, commentID, acceptedID pgtype.UUID) {
	if !issueID.Valid || !commentID.Valid || !acceptedID.Valid {
		return
	}
	if _, err := h.Queries.LinkTriggeredTasksToDelegationReturn(ctx, db.LinkTriggeredTasksToDelegationReturnParams{
		IssueID:            issueID,
		TriggerCommentID:   commentID,
		DelegationReturnID: acceptedID,
	}); err != nil {
		slog.Warn("link comment-triggered tasks to accepted delegation return failed",
			"issue_id", util.UUIDToString(issueID),
			"comment_id", util.UUIDToString(commentID),
			"delegation_return_id", util.UUIDToString(acceptedID),
			"error", err,
		)
	}
}
