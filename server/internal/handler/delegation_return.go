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

// recordDelegationReturn records a prepared address for one exact assignment
// slot through the caller's transaction and returns the accepted row's id. It
// is a no-op when the dispatch needs no address. A managed address is locked
// and re-checked by delegation.Ensure, so a source run that completed after
// authentication cannot still accept this delegation.
func recordDelegationReturn(ctx context.Context, q *db.Queries, issue db.Issue, assignee delegation.Assignee, address *delegation.Address, createdBy string) (pgtype.UUID, error) {
	if address == nil {
		return pgtype.UUID{}, nil
	}
	accepted, err := delegation.Ensure(ctx, q, util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), assignee, *address, createdBy)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return accepted.ID, nil
}

// commentTriggerAssignee names the assignment slot one computed comment trigger
// dispatches to. A squad leader is recorded against the squad, not against the
// leader's own agent id, so a direct assignment to that leader cannot borrow the
// squad's accepted returns.
func commentTriggerAssignee(issue db.Issue, trigger commentAgentTrigger) delegation.Assignee {
	if trigger.Squad != nil {
		return delegation.Assignee{Type: "squad", ID: uuidToString(trigger.Squad.ID)}
	}
	if trigger.Source == commentTriggerSourceIssueAssignee {
		return delegation.Assignee{Type: issue.AssigneeType.String, ID: uuidToString(issue.AssigneeID)}
	}
	return delegation.Assignee{Type: "agent", ID: uuidToString(trigger.Agent.ID)}
}

// recordCommentDelegationReturns records the author's accepted return for every
// executable trigger in the comment transaction. The result is keyed by the
// executing agent id and carries the explicit return set for that assignment
// slot, so a multi-target comment does not attach one target's relationship to
// another target's run. A nil address (member author) returns nil.
func recordCommentDelegationReturns(ctx context.Context, q *db.Queries, issue db.Issue, address *delegation.Address, plan commentTriggerPlan, createdBy string) (map[string][]pgtype.UUID, error) {
	if address == nil || len(plan.triggers) == 0 {
		return nil, nil
	}
	returns := make(map[string][]pgtype.UUID, len(plan.triggers))
	for _, trigger := range plan.triggers {
		assignee := commentTriggerAssignee(issue, trigger)
		if _, err := delegation.Ensure(ctx, q, util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), assignee, *address, createdBy); err != nil {
			return nil, err
		}
		ids, err := delegation.ReturnIDsForAssignee(ctx, q, issue.WorkspaceID, issue.ID, assignee)
		if err != nil {
			return nil, err
		}
		returns[uuidToString(trigger.Agent.ID)] = ids
	}
	return returns, nil
}

// collectDispatchReturns builds the explicit accepted set a run about to be
// created must carry:
//
//   - the target assignment's own accepted relationships (which include the
//     current dispatcher's row just recorded, plus a backlog row for this
//     assignee that no run consumed yet);
//   - the replaced assignment's relationships for an executor reassignment, so
//     the original recipients still receive the result; and
//   - the source run's complete set for a rerun or continuation.
//
// It never uses every historical return on the issue or every unlinked row:
// those would attach another dispatcher's or another worker's relationship.
func (h *Handler) collectDispatchReturns(ctx context.Context, q *db.Queries, issue db.Issue, assignee delegation.Assignee, previous *delegation.Assignee, sourceTaskID pgtype.UUID) ([]pgtype.UUID, error) {
	ids, err := delegation.ReturnIDsForAssignee(ctx, q, issue.WorkspaceID, issue.ID, assignee)
	if err != nil {
		return nil, err
	}
	sets := [][]pgtype.UUID{ids}
	if previous != nil {
		previousIDs, err := delegation.ReturnIDsForAssignee(ctx, q, issue.WorkspaceID, issue.ID, *previous)
		if err != nil {
			return nil, err
		}
		sets = append(sets, previousIDs)
	}
	if sourceTaskID.Valid {
		inherited, err := delegation.RunReturnIDs(ctx, q, sourceTaskID)
		if err != nil {
			return nil, err
		}
		sets = append(sets, inherited)
	}
	return delegation.UnionReturnIDs(sets...), nil
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
		errors.Is(err, delegation.ErrReturnSourceClosed):
		return err
	default:
		return nil
	}
}

// writeDelegationReturnError reports a refused delegation with a stable code so
// an agent can distinguish "retry after the session pin lands" from "your
// address is wrong" and from "your run has already finished".
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
