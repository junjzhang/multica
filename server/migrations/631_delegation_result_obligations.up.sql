ALTER TABLE agent_task_delegation_return
    ADD COLUMN notification_delivery_id UUID,
    ADD COLUMN notification_task_id UUID,
    ADD COLUMN notification_error TEXT,
    ADD COLUMN materialized_at TIMESTAMPTZ,
    ADD CONSTRAINT delegation_result_single_transport
        CHECK (num_nonnulls(notification_delivery_id, notification_task_id) <= 1);

ALTER TABLE agent_task_queue
    ADD COLUMN delegation_outcome_final BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN resource_control_required BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN terminal_reported_at TIMESTAMPTZ;

UPDATE agent_task_queue SET delegation_outcome_final = true
WHERE status = 'completed';
