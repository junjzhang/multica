ALTER TABLE agent_mailbox_message
    DROP COLUMN IF EXISTS delivery_failure_streak,
    DROP COLUMN IF EXISTS delivery_retry_class;
