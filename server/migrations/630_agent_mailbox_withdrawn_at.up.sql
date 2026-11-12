-- A notification its producer withdrew before any host accepted it. A pending
-- row becomes failed in the same transaction, so it can never be claimed. A
-- leased or unknown row keeps its transport state because the host may already
-- have acted; the marker is what stops a later known no-effect answer from
-- returning that row to claimable delivery and sending it again.
ALTER TABLE agent_mailbox_message ADD COLUMN withdrawn_at TIMESTAMPTZ;
