CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_mailbox_issue
    ON agent_mailbox_message (issue_id, created_at, id);
