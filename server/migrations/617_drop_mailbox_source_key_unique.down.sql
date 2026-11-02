CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_agent_mailbox_delivery_idempotency
    ON agent_mailbox_message (workspace_id, recipient_type, recipient_agent_id, source_type, source_key)
    WHERE source_key IS NOT NULL;
