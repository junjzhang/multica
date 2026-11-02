-- Which accepted delegation return a dispatched run belongs to. The result
-- submission transaction reads this instead of guessing the recipient from the
-- issue's coordinator, the latest agent+issue binding, or a folded comment.
-- No foreign key: queue rows are hot-path and their dependent records are
-- cleaned up in application code.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS delegation_return_id UUID;

COMMENT ON COLUMN agent_task_queue.delegation_return_id IS
    'The accepted issue_delegation_return this run was dispatched under. Set only when a fresh task is inserted for a delegated issue; comment coalescing and reruns inherit the existing task''s value and never overwrite it.';
