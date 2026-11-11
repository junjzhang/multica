CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_delegation_result_delivery
ON agent_task_delegation_return (notification_delivery_id)
WHERE notification_delivery_id IS NOT NULL;
