ALTER TABLE agent_mailbox_message
    DROP COLUMN IF EXISTS target_session_id,
    DROP COLUMN IF EXISTS notification_key;
