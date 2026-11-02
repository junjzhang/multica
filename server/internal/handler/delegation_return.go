package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

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
// host-session reference. Auth strips client-supplied identity headers, so an
// agent actor without one of those two stamped sources has no address and must
// fail closed rather than fall back to a legacy header pair.
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
		return delegation.Address{}, delegation.ErrReturnMissing
	}
}

// prepareCommentDelegationReturn validates the author's address when this
// comment will start at least one executable native run. A mention that
// resolved only to blocked targets (runtime unavailable, archived, ...) has no
// executable trigger, so the comment keeps its per-target blocked outcome
// instead of being refused. Discussions and member comments need no address.
func (h *Handler) prepareCommentDelegationReturn(r *http.Request, issue db.Issue, actorType, actorID string, plan commentTriggerPlan) (*delegation.Address, error) {
	if len(plan.triggers) == 0 {
		return nil, nil
	}
	return h.prepareDelegationReturn(r, issue, actorType, actorID, true, pgtype.Text{String: "agent", Valid: true})
}

// prepareDelegationReturn validates the current actor's address before a
// business write, without persisting anything. The address is recorded later,
// inside that write's transaction, so a rejected write leaves no phantom
// accepted return. It returns nil when this dispatch needs no address.
func (h *Handler) prepareDelegationReturn(r *http.Request, issue db.Issue, actorType, actorID string, willRun bool, assigneeType pgtype.Text) (*delegation.Address, error) {
	if !willRun || !delegationReturnRequired(actorType, assigneeType) {
		return nil, nil
	}
	// An issue-level return is not evidence for THIS dispatch: always resolve
	// the current trusted actor context. A stale or second dispatcher's row
	// must never let a new delegation skip the address check.
	address, err := h.resolveDelegationAddress(r, util.UUIDToString(issue.WorkspaceID), actorType, actorID)
	if err != nil {
		return nil, err
	}
	return &address, nil
}

// recordDelegationReturn records a prepared address through the caller's
// transaction and returns the accepted row's id. It is a no-op when the
// dispatch needs no address.
func recordDelegationReturn(ctx context.Context, q *db.Queries, issue db.Issue, address *delegation.Address, createdBy string) (pgtype.UUID, error) {
	if address == nil {
		return pgtype.UUID{}, nil
	}
	accepted, err := delegation.Ensure(ctx, q, util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), *address, createdBy)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return accepted.ID, nil
}

// collectDispatchReturns builds the complete set of accepted relationships the
// run about to be created must carry:
//
//   - the current dispatcher's accepted return (when it is an agent);
//   - the source run's complete set for a rerun or continuation;
//   - every accepted return on the issue for an executor reassignment, so the
//     original recipients still receive the result; or, otherwise, only
//     relationships no run carries yet (a backlog delegation whose execution
//     is only now starting).
func (h *Handler) collectDispatchReturns(ctx context.Context, issue db.Issue, actorReturnID, sourceTaskID pgtype.UUID, inheritAll bool) ([]pgtype.UUID, error) {
	ids := make([]pgtype.UUID, 0, 4)
	if actorReturnID.Valid {
		ids = append(ids, actorReturnID)
	}
	if sourceTaskID.Valid {
		inherited, err := delegation.RunReturnIDs(ctx, h.Queries, sourceTaskID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, inherited...)
	}
	var err error
	if inheritAll {
		var all []pgtype.UUID
		all, err = delegation.IssueReturnIDs(ctx, h.Queries, issue.WorkspaceID, issue.ID)
		ids = append(ids, all...)
	} else {
		var unlinked []pgtype.UUID
		unlinked, err = delegation.UnlinkedReturnIDs(ctx, h.Queries, issue.WorkspaceID, issue.ID)
		ids = append(ids, unlinked...)
	}
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// linkCommentTaskReturns attaches a comment's accepted relationship to the runs
// that comment became part of, including a pending run it was folded into. The
// relationship itself already committed with the comment; a link failure is
// reported rather than rolled back into the saved comment.
func (h *Handler) linkCommentTaskReturns(ctx context.Context, issue db.Issue, commentID, returnID pgtype.UUID) {
	if err := delegation.LinkComment(ctx, h.Queries, issue.ID, commentID, returnID); err != nil {
		slog.Warn("link comment tasks to accepted delegation return failed",
			"issue_id", util.UUIDToString(issue.ID),
			"comment_id", util.UUIDToString(commentID),
			"delegation_return_id", util.UUIDToString(returnID),
			"error", err)
	}
}

// asDelegationReturnError returns err when it is a delegation gate refusal, so
// callers that ran the gate inside a write transaction can render the stable
// code instead of a generic 500.
func asDelegationReturnError(err error) error {
	switch {
	case errors.Is(err, delegation.ErrReturnMissing),
		errors.Is(err, delegation.ErrReturnInvalid),
		errors.Is(err, delegation.ErrReturnNotReady):
		return err
	default:
		return nil
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
