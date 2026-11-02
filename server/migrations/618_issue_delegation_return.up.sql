-- A delegated issue names each actor its results return to and the address that
-- actor accepted when it dispatched work on the issue. The address is either a
-- registered external host session or the managed run that dispatched the
-- work; an Agent identity alone is not an address.
--
-- The row is keyed by (workspace, issue, dispatcher agent): every real
-- dispatcher keeps its own accepted relationship, so a second agent delegating
-- on the same issue is never addressed back to the first. The first accepted
-- delegation for one key owns that key's recipient; an ordinary reassignment,
-- rerun, or folded comment reuses it, and only explicit takeover changes the
-- external binding. The delegated issue and this row are written in the same
-- transaction.
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
