CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_agent_mailbox_notification_key
    ON agent_mailbox_message (workspace_id, notification_key)
    WHERE notification_key IS NOT NULL;
