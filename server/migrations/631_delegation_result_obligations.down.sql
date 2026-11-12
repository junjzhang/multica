ALTER TABLE agent_task_delegation_return
    DROP CONSTRAINT IF EXISTS delegation_result_single_transport,
    DROP COLUMN IF EXISTS notification_delivery_id,
    DROP COLUMN IF EXISTS notification_task_id,
    DROP COLUMN IF EXISTS notification_error,
    DROP COLUMN IF EXISTS materialized_at;
ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS delegation_outcome_final,
    DROP COLUMN IF EXISTS resource_control_required,
    DROP COLUMN IF EXISTS terminal_reported_at;
