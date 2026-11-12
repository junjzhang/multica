-- Rolling back removes the state, so record the unconfirmed rows as terminal
-- failures instead of leaving a value the old constraint rejects. The delivery
-- ids and host receipts stay untouched; recovery is a manual review.
UPDATE agent_mailbox_message SET delivery_status = 'failed' WHERE delivery_status = 'unknown';

ALTER TABLE agent_mailbox_message
    DROP CONSTRAINT IF EXISTS agent_mailbox_message_delivery_status_check;

ALTER TABLE agent_mailbox_message
    ADD CONSTRAINT agent_mailbox_message_delivery_status_check
    CHECK (delivery_status IN ('pending', 'leased', 'delivered', 'failed'));
