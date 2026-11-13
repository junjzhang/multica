CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_mailbox_recipient
    ON agent_mailbox_message (recipient_agent_id, created_at, id);
