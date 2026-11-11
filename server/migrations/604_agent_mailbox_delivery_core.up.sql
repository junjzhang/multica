-- Evolve the external-agent mailbox into the delivery core's single storage.
--
-- issue_id/comment_id stay as the legacy comment projection the mailbox API,
-- daemon claim and issue detail already read. They become optional so a
-- generic delivery (CI run, background wait) no longer has to invent an issue
-- or comment. source_type/source_key/kind/payload are the core's immutable
-- fields; for old rows they are backfilled from the comment projection so
-- existing mailbox UUIDs and receipts are untouched.
ALTER TABLE agent_mailbox_message
    ALTER COLUMN issue_id DROP NOT NULL,
    ALTER COLUMN comment_id DROP NOT NULL,
    ALTER COLUMN sender_type DROP NOT NULL,
    ALTER COLUMN sender_id DROP NOT NULL;

ALTER TABLE agent_mailbox_message
    ADD COLUMN source_type TEXT NOT NULL DEFAULT 'comment',
    ADD COLUMN source_key TEXT,
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'comment_mention',
    ADD COLUMN recipient_type TEXT NOT NULL DEFAULT 'agent',
    ADD COLUMN payload JSONB;

-- Legacy rows are comment deliveries: their idempotency key is derived from the
-- existing (recipient_agent_id, comment_id) unique key, and their immutable
-- payload is the stable reference document the claim path already uses.
UPDATE agent_mailbox_message
SET source_key = comment_id::text,
    payload = jsonb_build_object(
        'schema', 'comment_mention.v1',
        'issue_id', issue_id,
        'comment_id', comment_id
    )
WHERE comment_id IS NOT NULL;
