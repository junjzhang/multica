-- An ambiguous host outcome needs its own delivery state. `failed` would claim
-- the request was refused, and `pending` would make it deliverable again; both
-- are wrong when the host may already have accepted the request. `unknown`
-- rows are re-claimed for read-only receipt verification only, and the worker
-- never sends them a second time.
--
-- Conditional DDL keeps a skipped or re-run migration idempotent.
ALTER TABLE agent_mailbox_message
    DROP CONSTRAINT IF EXISTS agent_mailbox_message_delivery_status_check;

ALTER TABLE agent_mailbox_message
    ADD CONSTRAINT agent_mailbox_message_delivery_status_check
    CHECK (delivery_status IN ('pending', 'leased', 'delivered', 'failed', 'unknown'));
