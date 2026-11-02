-- The accepted return relationships a dispatched run carries. A run has at
-- least the relationship of the actor that dispatched it; when several
-- dispatchers' requests coalesce into one run, the run keeps every one of
-- them, so a result can be delivered to each party instead of silently only
-- the first. A rerun or executor reassignment copies the source run's set.
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
