CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_delegation_result_task
ON agent_task_delegation_return (notification_task_id)
WHERE notification_task_id IS NOT NULL;
