package handler

import (
	"context"
	"errors"
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
		// A session-bound credential is addressed by its own session, never by
		// a header the caller chose. Legacy unbound tokens keep the explicit
		// header so existing sessions keep working until they are reissued.
		if sessionID, ok := externalActorSessionID(r); ok {
			return delegation.ResolveExternal(r.Context(), h.Queries, workspaceID, actorID, util.UUIDToString(sessionID))
		}
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

// recordDelegationReturn records a prepared address for one (issue, dispatcher)
// through the caller's transaction and returns the accepted row's id. A managed
// address is locked and re-checked by delegation.Ensure, so a source run that
// completed after authentication cannot still accept this delegation.
func recordDelegationReturn(ctx context.Context, q *db.Queries, issue db.Issue, address *delegation.Address, createdBy string) (delegation.ReturnIntent, error) {
	if address == nil {
		return delegation.ReturnIntent{}, nil
	}
	accepted, err := delegation.Ensure(ctx, q, util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), *address, createdBy)
	if err != nil {
		return delegation.ReturnIntent{}, err
	}
	return delegation.IntentForAddress(accepted, *address), nil
}

// recordCommentDelegationReturn records the comment's accepted return in the
// comment transaction and persists the comment -> return relation that every
// later run path reads. A nil address (member author) is a no-op.
func recordCommentDelegationReturn(ctx context.Context, q *db.Queries, issue db.Issue, commentID pgtype.UUID, address *delegation.Address, createdBy string) (delegation.ReturnIntent, error) {
	if address == nil {
		return delegation.ReturnIntent{}, nil
	}
	accepted, err := delegation.Ensure(ctx, q, util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), *address, createdBy)
	if err != nil {
		return delegation.ReturnIntent{}, err
	}
	intent := delegation.IntentForAddress(accepted, *address)
	if err := delegation.LinkCommentReturn(ctx, q, commentID, intent); err != nil {
		return delegation.ReturnIntent{}, err
	}
	return intent, nil
}

// assignmentDispatch is the exact intent set one assignment write accepts:
// the current actor's accepted return, the target slot's still-unstarted
// backlog intent, and the replaced assignment's intent/run relationships.
type assignmentDispatch struct {
	target         delegation.Assignee
	previous       *delegation.Assignee
	previousIntent *delegation.AssignmentIntent
	returns        []delegation.ReturnIntent
}

// prepareAssignmentDispatch computes the accepted set for one assignment
// without writing. It deliberately never lists every return on the issue or
// every run of the target worker: a backlog slot contributes only its own
// unconsumed intent, and a reassignment contributes only the replaced
// assignment's set and its corresponding run.
func (h *Handler) prepareAssignmentDispatch(ctx context.Context, q *db.Queries, issue db.Issue, target delegation.Assignee, previous *delegation.Assignee, accepted delegation.ReturnIntent) (*assignmentDispatch, error) {
	dispatch := &assignmentDispatch{target: target, previous: previous}
	sets := [][]delegation.ReturnIntent{{accepted}}

	current, err := delegation.LoadAssignmentIntent(ctx, q, issue.ID, target)
	if err != nil {
		return nil, err
	}
	if current != nil && !current.TaskID.Valid {
		// The backlog intent for exactly this slot has not started yet.
		sets = append(sets, current.Returns)
	}
	if previous != nil && !sameAssignee(*previous, target) {
		intent, err := delegation.LoadAssignmentIntent(ctx, q, issue.ID, *previous)
		if err != nil {
			return nil, err
		}
		dispatch.previousIntent = intent
		if intent != nil {
			sets = append(sets, intent.Returns)
			if intent.TaskID.Valid {
				inherited, err := delegation.RunReturns(ctx, q, intent.TaskID)
				if err != nil {
					return nil, err
				}
				sets = append(sets, inherited)
			}
		}
	}
	dispatch.returns = delegation.UnionReturns(sets...)
	return dispatch, nil
}

// commitAssignmentIntent persists the exact set before the run is created, then
// records the run that carries it. A replaced assignment's intent is removed so
// a much later assignment back to that slot cannot resurrect a stale
// dispatcher.
func commitAssignmentIntent(ctx context.Context, q *db.Queries, issue db.Issue, dispatch *assignmentDispatch, taskID pgtype.UUID) error {
	if dispatch == nil {
		return nil
	}
	if dispatch.previous != nil && !sameAssignee(*dispatch.previous, dispatch.target) {
		if err := delegation.DeleteAssignmentIntent(ctx, q, issue.ID, *dispatch.previous); err != nil {
			return err
		}
	}
	if len(delegation.UnionReturns(dispatch.returns)) == 0 {
		// A human assignment with no prior delegated intent dispatches work but
		// accepts no return address. Do not leave an empty intent behind.
		return nil
	}
	return delegation.StoreAssignmentIntent(ctx, q, issue.WorkspaceID, issue.ID, dispatch.target, dispatch.returns, taskID)
}

func sameAssignee(a, b delegation.Assignee) bool {
	return a.Type == b.Type && a.ID == b.ID
}

// previousAssignee returns the replaced assignment slot for a reassignment.
// Only an agent/squad assignment owns returns; a member or unassigned issue has
// none to preserve.
func previousAssignee(issue db.Issue) *delegation.Assignee {
	if !issue.AssigneeID.Valid {
		return nil
	}
	if issue.AssigneeType.String != "agent" && issue.AssigneeType.String != "squad" {
		return nil
	}
	return &delegation.Assignee{Type: issue.AssigneeType.String, ID: uuidToString(issue.AssigneeID)}
}

// asDelegationReturnError returns err when it is a delegation gate refusal, so
// callers that ran the gate inside a write transaction can render the stable
// code instead of a generic 500.
func asDelegationReturnError(err error) error {
	switch {
	case errors.Is(err, delegation.ErrReturnMissing),
		errors.Is(err, delegation.ErrReturnInvalid),
		errors.Is(err, delegation.ErrReturnNotReady),
		errors.Is(err, delegation.ErrReturnSourceClosed),
		errors.Is(err, delegation.ErrReturnConflict):
		return err
	default:
		return nil
	}
}

// writeDelegationReturnError reports a refused delegation with a stable code so
// an agent can distinguish "retry after the session pin lands" from "your
// address is wrong", from "your run has already finished", and from an
// existing different physical session that needs an explicit takeover.
func writeDelegationReturnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, delegation.ErrReturnNotReady):
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":  "delegation_return_not_ready",
			"error": "The delegating run has not recorded its provider session yet. Retry the delegation after this run's session is persisted; no child work was created.",
		})
	case errors.Is(err, delegation.ErrReturnSourceClosed):
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":  "delegation_return_source_closed",
			"error": "The delegating run is no longer active. No child work was created; delegate from an active run instead.",
		})
	case errors.Is(err, delegation.ErrReturnConflict):
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":  "delegation_return_conflict",
			"error": "The return session, source scope, or caller authority conflicts with accepted work on this issue. The existing return address remains fixed; this dispatch was not accepted.",
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
