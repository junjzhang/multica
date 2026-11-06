-- Fixed coordinator (the entity unresolved disagreements escalate to) and the
-- human-granted merge policy live on the issue so every issue read can project
-- them without a join. Both are written only by an explicit issue update and
-- participate in the native revision/optimistic-concurrency contract.
--
-- merge_allowed defaults to false: authorization to implement is never
-- authorization to merge. This stores policy only; this fork has no controlled
-- merge executor, so it does not gate arbitrary external `gh` shell usage.
ALTER TABLE issue
    ADD COLUMN coordinator_type TEXT,
    ADD COLUMN coordinator_id UUID,
    ADD COLUMN merge_allowed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN merge_repository_url TEXT,
    ADD COLUMN merge_target_branch TEXT,
    ADD COLUMN merge_granted_by UUID,
    ADD COLUMN merge_granted_at TIMESTAMPTZ;

ALTER TABLE issue
    ADD CONSTRAINT issue_coordinator_check CHECK (
        (coordinator_type IS NULL AND coordinator_id IS NULL)
        OR (coordinator_type IN ('member', 'agent') AND coordinator_id IS NOT NULL)
    );
