-- The mailbox idempotency key: one request per recipient and comment.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_agent_mailbox_recipient_comment
    ON agent_mailbox_message (recipient_agent_id, comment_id);
