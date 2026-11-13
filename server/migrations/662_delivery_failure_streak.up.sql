ALTER TABLE agent_mailbox_message
    ADD COLUMN delivery_failure_streak INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN delivery_retry_class TEXT NOT NULL DEFAULT '';
