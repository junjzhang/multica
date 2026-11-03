-- An accepted delegation return is the exact address one dispatcher's results
-- go back to on one issue. The address is either a registered external host
-- session or the managed run whose provider session dispatched the work; an
-- Agent identity alone is not an address.
--
-- The row is keyed by (workspace, issue, dispatcher agent): distinct actors on
-- the same issue keep their own accepted address, and one actor keeps one
-- address for the issue independent of which worker it dispatches to. Which
-- dispatch intent a run carries is recorded separately (comment and assignment
-- intent joins below), so a worker's independent request never inherits
-- another dispatcher's recipient. The intended business write and this row
-- commit in the same transaction.
CREATE TABLE issue_delegation_return (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    dispatcher_agent_id UUID NOT NULL,
    return_kind TEXT NOT NULL
        CHECK (return_kind IN ('external_session', 'managed_task')),
    external_session_id UUID,
    source_task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (return_kind = 'external_session'
            AND external_session_id IS NOT NULL
            AND source_task_id IS NULL)
        OR
        (return_kind = 'managed_task'
            AND source_task_id IS NOT NULL
            AND external_session_id IS NULL)
    )
);

-- A comment is the stable business identity of a dispatch intent. The join
-- records which accepted return relationships that comment created, in the
-- same transaction as the comment. New runs, folds, completion replay and
-- edit/cancel retrigger all read this relation; none of them reconstruct a
-- dispatcher from the issue's current assignee, a worker's history, or an
-- in-memory trigger result.
CREATE TABLE comment_delegation_return (
    comment_id UUID NOT NULL,
    delegation_return_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (comment_id, delegation_return_id)
);

-- An assignment is a dispatch intent before it has a run (backlog) and while
-- it runs. The header names the exact assignment slot and, once a run exists,
-- the task that carries it. The return join is the set of accepted
-- relationships that assignment carries; a reassignment copies the replaced
-- assignment's set plus the reassigning actor's own relationship.
--
-- One issue has one intent per assignment slot, keyed by the slot rather than
-- by the issue: a backlog intent survives an unrelated worker's comment run
-- and is consumed only when that exact slot starts execution.
CREATE TABLE issue_assignment_delegation (
    issue_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    assignee_type TEXT NOT NULL CHECK (assignee_type IN ('agent', 'squad')),
    assignee_id UUID NOT NULL,
    task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, assignee_type, assignee_id)
);

CREATE TABLE issue_assignment_delegation_return (
    issue_id UUID NOT NULL,
    assignee_type TEXT NOT NULL,
    assignee_id UUID NOT NULL,
    delegation_return_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, assignee_type, assignee_id, delegation_return_id)
);
