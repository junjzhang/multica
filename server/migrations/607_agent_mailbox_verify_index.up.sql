-- Verification-only rows are claimed by (status, next check time). The existing
-- claim index covers pending/leased only, so keep receipt checks off a full
-- scan without rewriting that index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_mailbox_verify
    ON agent_mailbox_message (delivery_status, next_attempt_at)
    WHERE delivery_status = 'unknown';
