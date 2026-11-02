-- A delivery's address is the physical session it was produced for, frozen at
-- enqueue. recipient_agent_id stays the business projection (mailbox listing,
-- issue detail); it is no longer the routing key.
ALTER TABLE agent_mailbox_message
    ADD COLUMN target_session_id UUID,
    ADD COLUMN notification_key TEXT;

-- Every existing row addressed an agent whose session was unique at this
-- revision (uq_external_agent_session_agent), so the mapping below is the
-- row's recorded destination, not a guess from a later rebind.
UPDATE agent_mailbox_message m
SET target_session_id = s.id
FROM external_agent_session s
WHERE m.target_session_id IS NULL
  AND s.agent_id = m.recipient_agent_id;

-- A row with no registered session has no provable destination. Keep its id
-- and receipt, make the reason visible, and never re-send it: the claim path
-- requires target_session_id, so these rows stay quarantined.
UPDATE agent_mailbox_message
SET last_error = 'legacy delivery has no registered external session; preserved with its original id and receipt and never re-sent'
WHERE target_session_id IS NULL
  AND delivery_status <> 'delivered'
  AND last_error IS NULL;

-- Existing external agents already served the issues they participated in.
-- Their session was the only one, so binding those pairs preserves exactly
-- the destination the old agent-keyed claim resolved.
INSERT INTO external_agent_session_binding (workspace_id, agent_id, issue_id, session_id)
SELECT s.workspace_id, s.agent_id, i.id, s.id
FROM external_agent_session s
JOIN issue i ON i.workspace_id = s.workspace_id
WHERE EXISTS (
        SELECT 1 FROM comment c
        WHERE c.issue_id = i.id
          AND c.author_type = 'agent'
          AND c.author_id = s.agent_id
          AND c.deleted_at IS NULL
    )
   OR (i.creator_type = 'agent' AND i.creator_id = s.agent_id)
   OR (i.assignee_type = 'agent' AND i.assignee_id = s.agent_id)
   OR (i.coordinator_type = 'agent' AND i.coordinator_id = s.agent_id)
ON CONFLICT DO NOTHING;
