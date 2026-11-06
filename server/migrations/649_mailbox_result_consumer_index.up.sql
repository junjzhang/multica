CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mailbox_result_consumer
ON agent_mailbox_message (handled_by_task_id)
WHERE handled_by_task_id IS NOT NULL;
