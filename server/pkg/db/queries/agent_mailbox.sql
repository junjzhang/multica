-- name: CreateAgentMailboxMessage :one
-- The unique (recipient_agent_id, comment_id) key is the idempotency boundary:
-- a replayed delivery attempt returns no row and the caller re-reads the
-- existing message instead of creating a duplicate request.
INSERT INTO agent_mailbox_message (
    id, workspace_id, issue_id, comment_id, recipient_agent_id, source_session_id,
    sender_type, sender_id
) VALUES (
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()), @workspace_id, @issue_id,
    @comment_id, @recipient_agent_id, sqlc.narg('source_session_id'),
    @sender_type, @sender_id
)
ON CONFLICT (recipient_agent_id, comment_id) DO NOTHING
RETURNING *;

-- name: GetAgentMailboxMessage :one
SELECT * FROM agent_mailbox_message WHERE id = $1 AND workspace_id = $2;

-- name: GetAgentMailboxMessageByComment :one
SELECT * FROM agent_mailbox_message
WHERE recipient_agent_id = @recipient_agent_id AND comment_id = @comment_id;

-- name: CreateDelivery :one
-- The delivery core's only insert. The unique (workspace_id, recipient_type,
-- recipient_agent_id, source_type, source_key) index is the idempotency
-- boundary; a conflicting row returns nothing so the caller can re-read it and
-- compare the immutable content.
--
-- The conflict target is deliberately omitted so the legacy
-- (recipient_agent_id, comment_id) index is absorbed too. Rows written before
-- the core can have source_key NULL, which the partial idempotency index does
-- not cover; only the legacy index identifies them, and the caller recovers
-- their original UUID through GetLegacyCommentDelivery. Generic sources cannot
-- create that ambiguity: EnqueueRequest.Validate rejects a comment reference
-- unless the source itself is "comment".
--
-- issue_id/comment_id/sender_* are the legacy comment projection; generic
-- deliveries leave them NULL.
INSERT INTO agent_mailbox_message (
    id, workspace_id, issue_id, comment_id, recipient_agent_id, recipient_type,
    source_session_id, sender_type, sender_id, source_type, source_key, kind, payload
) VALUES (
    @id, @workspace_id, sqlc.narg('issue_id'), sqlc.narg('comment_id'),
    @recipient_agent_id, @recipient_type, sqlc.narg('source_session_id'),
    sqlc.narg('sender_type'), sqlc.narg('sender_id'), @source_type, @source_key,
    @kind, sqlc.narg('payload')
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetDelivery :one
SELECT * FROM agent_mailbox_message WHERE id = @id AND workspace_id = @workspace_id;

-- name: GetDeliveryByIdempotencyKey :one
SELECT * FROM agent_mailbox_message
WHERE workspace_id = @workspace_id
  AND recipient_type = @recipient_type
  AND recipient_agent_id = @recipient_agent_id
  AND source_type = @source_type
  AND source_key = @source_key;

-- name: GetLegacyCommentDelivery :one
-- Rows written before the delivery core (and the old test-only insert path)
-- have no source_key. The original (recipient_agent_id, comment_id) unique key
-- remains their identity, so a replay returns the original mailbox UUID instead
-- of creating a second request.
SELECT * FROM agent_mailbox_message
WHERE recipient_agent_id = @recipient_agent_id AND comment_id = @comment_id;

-- name: ListAgentMailboxMessages :many
-- Keyset pagination in (created_at, id) order. The cursor is opaque to the
-- client; both halves are bound, so a page boundary can never repeat or skip a
-- message created in the same millisecond.
--
-- Only comment projections are listed: the frozen DTO carries issue_id and
-- comment_id, and a generic delivery has neither. Old clients keep their exact
-- view; generic deliveries are read through the delivery core until a separate
-- projection exists.
SELECT * FROM agent_mailbox_message
WHERE recipient_agent_id = @recipient_agent_id
  AND workspace_id = @workspace_id
  AND source_type = 'comment'
  AND (
    sqlc.narg('after_created_at')::timestamptz IS NULL
    OR created_at > sqlc.narg('after_created_at')::timestamptz
    OR (
        created_at = sqlc.narg('after_created_at')::timestamptz
        AND id > sqlc.narg('after_id')::uuid
    )
  )
ORDER BY created_at ASC, id ASC
LIMIT @page_limit;

-- name: LockClaimableMailboxMessageIDs :many
-- Locks a bounded batch of deliverable messages. The claim is bound to the
-- verified daemon/owner: the recipient must be a live external agent whose
-- session names this daemon and owner, and whose workspace matches. A pending
-- message is claimable at its next attempt; an expired lease is reclaimable so
-- a crashed daemon does not strand the request.
SELECT m.id
FROM agent_mailbox_message m
JOIN external_agent_session s ON s.agent_id = m.recipient_agent_id
JOIN agent a ON a.id = m.recipient_agent_id
WHERE m.workspace_id = @workspace_id
  AND m.source_type = 'comment'
  AND s.workspace_id = @workspace_id
  AND a.workspace_id = @workspace_id
  AND a.execution_kind = 'external'
  AND a.archived_at IS NULL
  AND s.owner_user_id = @owner_user_id
  AND s.daemon_id = @daemon_id
  AND (
    (m.delivery_status = 'pending' AND m.next_attempt_at <= now())
    OR (m.delivery_status = 'leased' AND m.lease_expires_at <= now())
  )
ORDER BY m.next_attempt_at ASC, m.created_at ASC
LIMIT @row_limit
FOR UPDATE OF m SKIP LOCKED;

-- name: LeaseMailboxMessage :one
UPDATE agent_mailbox_message
SET delivery_status = 'leased',
    lease_token_hash = @lease_token_hash,
    lease_owner = @lease_owner,
    lease_expires_at = @lease_expires_at,
    attempts = attempts + 1,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: AckAgentMailboxDelivery :one
-- The token and daemon fence this ack to the exact lease that claimed the
-- message. `delivered`/`retry`/`failed` all clear the lease, so a stale ack
-- (or a second ack of the same lease) matches no row and cannot overwrite a
-- later lease.
UPDATE agent_mailbox_message
SET delivery_status = CASE WHEN @status = 'retry' THEN 'pending' ELSE @status END,
    delivered_at = CASE WHEN @status = 'delivered' THEN now() ELSE delivered_at END,
    host_receipt = COALESCE(sqlc.narg('host_receipt'), host_receipt),
    last_error = sqlc.narg('last_error'),
    next_attempt_at = CASE WHEN @status = 'retry' THEN @next_attempt_at ELSE next_attempt_at END,
    lease_token_hash = NULL,
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND delivery_status = 'leased'
  AND lease_token_hash = @lease_token_hash
  AND lease_expires_at > now()
  AND lease_owner = @daemon_id
RETURNING *;

-- name: AckAgentMailboxProcessing :one
-- Monotonic processing state: read can never undo handled, and handled is
-- terminal. Delivery state is deliberately untouched — receiving a message is
-- not executing it.
UPDATE agent_mailbox_message
SET processing_status = CASE
        WHEN processing_status = 'handled' THEN 'handled'
        WHEN @state::text = 'handled' THEN 'handled'
        ELSE 'read'
    END,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND recipient_agent_id = @recipient_agent_id
RETURNING *;

-- name: ListIssueMailboxDeliveries :many
-- Bounded recent projection for the issue response. Complete history is the
-- mailbox API's keyset pagination.
SELECT * FROM agent_mailbox_message
WHERE issue_id = @issue_id AND workspace_id = @workspace_id
ORDER BY created_at DESC, id DESC
LIMIT 50;

-- name: DeleteAgentMailboxMessagesForIssue :exec
DELETE FROM agent_mailbox_message WHERE issue_id = $1;

-- name: DeleteAgentMailboxMessagesForComment :exec
DELETE FROM agent_mailbox_message WHERE comment_id = $1;

-- name: ListIssueExternalParticipants :many
-- External agents that took part in this issue as author, creator, assignee or
-- coordinator. Computed from the whole issue, not from one comments page, so a
-- participant never disappears when their comment scrolls off a page.
SELECT a.* FROM agent a
WHERE a.workspace_id = @workspace_id
  AND a.execution_kind = 'external'
  AND (
    EXISTS (
        SELECT 1 FROM comment c
        WHERE c.issue_id = @issue_id
          AND c.author_type = 'agent'
          AND c.author_id = a.id
          AND c.deleted_at IS NULL
    )
    OR EXISTS (
        SELECT 1 FROM issue i
        WHERE i.id = @issue_id
          AND i.creator_type = 'agent'
          AND i.creator_id = a.id
    )
    OR EXISTS (
        SELECT 1 FROM issue i
        WHERE i.id = @issue_id
          AND i.assignee_type = 'agent'
          AND i.assignee_id = a.id
    )
    OR EXISTS (
        SELECT 1 FROM issue i
        WHERE i.id = @issue_id
          AND i.coordinator_type = 'agent'
          AND i.coordinator_id = a.id
    )
  )
ORDER BY a.created_at ASC, a.id ASC;
