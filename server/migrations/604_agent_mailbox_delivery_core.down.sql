-- Generic deliveries have no issue/comment, so they cannot satisfy the old
-- NOT NULL projection. Rollback is destructive for those rows by design; the
-- comment-backed mailbox history is preserved.
DELETE FROM agent_mailbox_message
WHERE issue_id IS NULL OR comment_id IS NULL OR sender_type IS NULL OR sender_id IS NULL;

ALTER TABLE agent_mailbox_message
    DROP COLUMN IF EXISTS payload,
    DROP COLUMN IF EXISTS recipient_type,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS source_key,
    DROP COLUMN IF EXISTS source_type;

ALTER TABLE agent_mailbox_message
    ALTER COLUMN issue_id SET NOT NULL,
    ALTER COLUMN comment_id SET NOT NULL,
    ALTER COLUMN sender_type SET NOT NULL,
    ALTER COLUMN sender_id SET NOT NULL;
