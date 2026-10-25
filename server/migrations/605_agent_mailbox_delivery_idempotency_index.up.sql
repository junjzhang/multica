-- One delivery per (scope, recipient, source, source key). Legacy rows were
-- backfilled from comment_id in 604, so the same comment can never be enqueued
-- twice under a different key. Single statement: CONCURRENTLY runs outside a
-- transaction, and the runner registers the invalid-index cleanup hook.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_agent_mailbox_delivery_idempotency
    ON agent_mailbox_message (workspace_id, recipient_type, recipient_agent_id, source_type, source_key)
    WHERE source_key IS NOT NULL;
