-- A delegated issue names the actor its result returns to and the address that
-- actor accepted when the delegation was dispatched. The address is either a
-- registered external host session or the managed run that dispatched the
-- work; an Agent identity alone is not an address.
--
-- The row is at most one per issue: the first accepted delegation owns the
-- result recipient. Reassigning the executor, folding a later comment into a
-- run, or an ordinary rerun never rewrites it; an explicit takeover changes the
-- external binding, not this record. The delegated issue and this row are
-- written in the same transaction, and a task launch never outruns it.
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
