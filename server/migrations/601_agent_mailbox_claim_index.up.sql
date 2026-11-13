CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_mailbox_claim
    ON agent_mailbox_message (delivery_status, next_attempt_at)
    WHERE delivery_status IN ('pending', 'leased');
