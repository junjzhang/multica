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

-- name: ListClaimableDeliveryIDs :many
-- Candidates for one claim round, at most one per recipient session. The
-- caller leases each candidate in its own transaction; the advisory lock it
-- takes per recipient serialises concurrent claimants, and the conditional
-- lease re-check makes a lost race a no-op. An inflated lease is reclaimable so
-- a crashed worker does not strand a request. An ambiguous row (unknown) is
-- claimable again only for receipt verification, never for a second send.
WITH ranked AS (
    SELECT m.id,
           m.recipient_agent_id,
           m.delivery_status,
           m.next_attempt_at,
           m.created_at,
           row_number() OVER (
               PARTITION BY m.recipient_agent_id
               ORDER BY m.next_attempt_at ASC, m.created_at ASC, m.id ASC
           ) AS recipient_rank
    FROM agent_mailbox_message m
    JOIN external_agent_session s ON s.agent_id = m.recipient_agent_id
    JOIN agent a ON a.id = m.recipient_agent_id
    WHERE m.workspace_id = @workspace_id
      AND (m.source_type = 'comment') = @comment_projection::boolean
      AND s.workspace_id = @workspace_id
      AND a.workspace_id = @workspace_id
      AND a.execution_kind = 'external'
      AND a.archived_at IS NULL
      AND s.owner_user_id = @owner_user_id
      AND s.daemon_id = @daemon_id
      AND (
          (m.delivery_status = 'pending' AND m.next_attempt_at <= now())
          OR (m.delivery_status = 'leased' AND m.lease_expires_at <= now())
          OR (m.delivery_status = 'unknown' AND m.next_attempt_at <= now())
      )
      AND NOT EXISTS (
          SELECT 1
          FROM agent_mailbox_message inflight
          WHERE inflight.recipient_agent_id = m.recipient_agent_id
            AND inflight.id <> m.id
            AND inflight.delivery_status = 'leased'
            AND inflight.lease_expires_at > now()
      )
)
SELECT id, recipient_agent_id, delivery_status
FROM ranked
WHERE recipient_rank = 1
ORDER BY next_attempt_at ASC, created_at ASC, id ASC
LIMIT @row_limit;

-- name: TryLockDeliveryRecipient :one
-- Transaction-scoped advisory lock that serialises claims for one recipient,
-- so two daemons cannot lease different rows for the same session at once.
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg('recipient_id')::text, 0)) AS locked;

-- name: LeaseClaimableDelivery :one
-- Conditional lease: only a row that is still eligible at this instant is
-- leased. A concurrent claimant that already leased or acknowledged it makes
-- this a no-op instead of overwriting its lease.
UPDATE agent_mailbox_message
SET delivery_status = 'leased',
    lease_token_hash = @lease_token_hash,
    lease_owner = @lease_owner,
    lease_expires_at = @lease_expires_at,
    attempts = attempts + 1,
    updated_at = now()
WHERE id = @id
  AND workspace_id = @workspace_id
  AND (
      (delivery_status = 'pending' AND next_attempt_at <= now())
      OR (delivery_status = 'leased' AND lease_expires_at <= now())
      OR (delivery_status = 'unknown' AND next_attempt_at <= now())
  )
RETURNING *;

-- name: RecordDeliveryOutcome :one
-- One fenced write for every outcome. The token, daemon and unexpired lease
-- fence the update: a stale or foreign ack matches no row and cannot overwrite
-- a later lease. The retry schedule separates a busy host (short fixed delay)
-- from an unreachable host or a not-yet-confirmed outcome (bounded failure
-- backoff). Only rows whose prior outcome was a known no-effect return to
-- claimable delivery; unknown rows are re-claimed for verification.
UPDATE agent_mailbox_message
SET delivery_status = @status,
    delivered_at = CASE WHEN @status = 'delivered' THEN now() ELSE delivered_at END,
    host_receipt = COALESCE(sqlc.narg('host_receipt'), host_receipt),
    last_error = sqlc.narg('last_error'),
    next_attempt_at = CASE
        WHEN @status = 'pending' AND @retry_class::text = 'busy' THEN now() + interval '5 seconds'
        WHEN @status = 'pending' THEN now() + LEAST(GREATEST(attempts::bigint * attempts::bigint * 15, 15), 900) * interval '1 second'
        WHEN @status = 'unknown' THEN now() + LEAST(GREATEST(attempts::bigint * attempts::bigint * 15, 15), 900) * interval '1 second'
        ELSE next_attempt_at
    END,
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
