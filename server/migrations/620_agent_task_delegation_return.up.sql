-- The accepted return relationships a dispatched run carries. A run has at
-- least the relationships of the intents it actually covers: the trigger
-- comment or the assignment it was created for; a fold adds the folded
-- comment's relationships; a rerun copies the exact source run's set plus the
-- rerunning actor's relationship; a reassignment copies the replaced
-- assignment's set plus the reassigning actor's relationship.
--
-- No foreign keys: queue rows are hot-path and their dependent records are
-- cleaned up in application code (workspace teardown deletes these rows with
-- the task and return they reference).
CREATE TABLE agent_task_delegation_return (
    task_id UUID NOT NULL,
    delegation_return_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, delegation_return_id)
);
