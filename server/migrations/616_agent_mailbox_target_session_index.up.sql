CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_mailbox_target_session
    ON agent_mailbox_message (target_session_id, delivery_status);
