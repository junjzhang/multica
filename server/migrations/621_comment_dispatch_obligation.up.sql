-- A durable dispatch obligation for one accepted comment target. The comment
-- row and its accepted return are written in the comment's transaction, so a
-- crash between accepting the comment and starting (or folding it into) a run
-- must not lose the target. This row is the minimal recovery record: it names
-- the exact issue, comment, executing agent, optional squad context, trigger
-- source and accepted return that the comment was accepted for. A sweeper
-- consumes pending rows idempotently and settles them once a run covers the
-- comment.
--
-- The obligation deliberately does not store mention text or re-resolve the
-- current assignee: replay executes the accepted intent, not a new delegation
-- guessed from mutable comment content or the issue's latest executor. Current
-- revocation and runtime admission are checked again by the consumer.
--
-- No foreign keys: comments, returns and queue rows are cleaned up in
-- application code (workspace teardown removes this row with them).
CREATE TABLE comment_dispatch_obligation (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    comment_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    squad_id UUID,
    trigger_source TEXT NOT NULL,
    delegation_return_id UUID,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'settled', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    settled_task_id UUID,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
