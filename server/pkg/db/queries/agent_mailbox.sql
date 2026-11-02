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
-- The delivery core's only insert. The unique (workspace_id, notification_key)
-- index is the idempotency boundary; a conflicting row returns nothing so the
-- caller can re-read it and compare the immutable content.
--
-- target_session_id is the frozen address of one physical session. A retry
-- keeps the row even if the producer now resolves a different session, so the
-- address and the delivery id never move under an existing notification.
--
-- The conflict target is deliberately omitted so the legacy
-- (recipient_agent_id, comment_id) index is absorbed too. Rows written before
-- the core have a NULL notification_key, which the partial index does not
-- cover; only the legacy index identifies them, and the caller recovers their
-- original UUID through GetLegacyCommentDelivery. Generic sources cannot
-- create that ambiguity: EnqueueRequest.Validate rejects a comment reference
-- unless the source itself is "comment".
--
-- issue_id/comment_id/sender_* are the legacy comment projection; generic
-- deliveries leave them NULL.
INSERT INTO agent_mailbox_message (
    id, workspace_id, issue_id, comment_id, recipient_agent_id, recipient_type,
    target_session_id, notification_key, source_session_id, sender_type, sender_id,
    source_type, source_key, kind, payload
) VALUES (
    @id, @workspace_id, sqlc.narg('issue_id'), sqlc.narg('comment_id'),
    @recipient_agent_id, @recipient_type, @target_session_id, @notification_key,
    sqlc.narg('source_session_id'),
    sqlc.narg('sender_type'), sqlc.narg('sender_id'), @source_type, @source_key,
    @kind, sqlc.narg('payload')
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetDelivery :one
SELECT * FROM agent_mailbox_message WHERE id = @id AND workspace_id = @workspace_id;

-- name: GetDeliveryByNotificationKey :one
-- The notification, not its issue or session, is the idempotency unit.
SELECT * FROM agent_mailbox_message
WHERE workspace_id = @workspace_id
  AND notification_key = @notification_key;

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
-- Candidates for one claim round, at most one per physical session. This list
-- is only a pre-lock hint: the conditional lease re-checks both row
-- eligibility and the same session's in-flight rows after taking the advisory
-- lock, so a candidate that lost a race is skipped. An inflated lease is
-- reclaimable so a crashed worker does not strand a request. An ambiguous row
-- (unknown) is claimable again only for receipt verification, never for a
-- second send.
--
-- Every source type shares this one list: a comment notification and a generic
-- event are delivered by the same worker and the same lease/outcome state
-- machine, so no source can be excluded from receipt recovery.
--
-- external_agent_session is joined by the frozen target_session_id, never by
-- the agent's current binding: the host's provider/host/session identity is
-- immutable, while daemon_id only decides which authorized worker can reach
-- it and may be reassigned without changing the target.
WITH ranked AS (
    SELECT m.id,
           m.target_session_id,
           m.delivery_status,
           m.next_attempt_at,
           m.created_at,
           row_number() OVER (
               PARTITION BY m.target_session_id
               ORDER BY m.next_attempt_at ASC, m.created_at ASC, m.id ASC
           ) AS target_rank
    FROM agent_mailbox_message m
    JOIN external_agent_session s ON s.id = m.target_session_id
    JOIN agent a ON a.id = s.agent_id
    WHERE m.workspace_id = @workspace_id
      AND m.target_session_id IS NOT NULL
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
          WHERE inflight.target_session_id = m.target_session_id
            AND inflight.id <> m.id
            AND inflight.delivery_status = 'leased'
            AND inflight.lease_expires_at > now()
      )
)
SELECT id, target_session_id, delivery_status
FROM ranked
WHERE target_rank = 1
ORDER BY next_attempt_at ASC, created_at ASC, id ASC
LIMIT @row_limit;

-- name: TryLockDeliveryTarget :one
-- Transaction-scoped advisory lock that serialises claims for one physical
-- session, so two daemons cannot lease different rows for the same session at
-- once. Two sessions of the same agent are different keys and proceed
-- independently.
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg('target_session_id')::text, 0)) AS locked;

-- name: LeaseClaimableDelivery :one
-- Conditional lease: only a row that is still eligible at this instant is
-- leased. A concurrent claimant that already leased or acknowledged it makes
-- this a no-op instead of overwriting its lease.
--
-- The in-flight check is repeated here, not only in the candidate list. Under
-- READ COMMITTED this statement takes a fresh snapshot after the caller's
-- per-session advisory lock, so it sees a lease another claim transaction
-- committed while this claim was being prepared. The candidate list alone only
-- proves the session had no in-flight row before the lock: two claim rounds for
-- different rows of one session could otherwise lease both.
--
-- Authorization is re-checked here too. The candidate list is a snapshot taken
-- before the advisory lock, and an archive or a session rebind can commit in
-- between; leasing must not trust that snapshot. This is the same owner,
-- workspace and daemon gate the candidate list applies, so an unauthorized
-- target can never be handed a lease. Withdrawal races are settled by the row
-- lock itself: this UPDATE and the withdrawal UPDATE serialise on the row, so
-- whichever commits first decides, and the loser re-evaluates the row.
UPDATE agent_mailbox_message
SET delivery_status = 'leased',
    lease_token_hash = @lease_token_hash,
    lease_owner = @lease_owner,
    lease_expires_at = @lease_expires_at,
    attempts = attempts + 1,
    updated_at = now()
WHERE agent_mailbox_message.id = @id
  AND agent_mailbox_message.workspace_id = @workspace_id
  AND agent_mailbox_message.target_session_id = @target_session_id
  AND (
      (agent_mailbox_message.delivery_status = 'pending' AND agent_mailbox_message.next_attempt_at <= now())
      OR (agent_mailbox_message.delivery_status = 'leased' AND agent_mailbox_message.lease_expires_at <= now())
      OR (agent_mailbox_message.delivery_status = 'unknown' AND agent_mailbox_message.next_attempt_at <= now())
  )
  AND EXISTS (
      SELECT 1
      FROM external_agent_session s
      JOIN agent a ON a.id = s.agent_id
      WHERE s.id = agent_mailbox_message.target_session_id
        AND s.workspace_id = agent_mailbox_message.workspace_id
        AND a.workspace_id = agent_mailbox_message.workspace_id
        AND a.execution_kind = 'external'
        AND a.archived_at IS NULL
        AND s.owner_user_id = @owner_user_id
        AND s.daemon_id = @daemon_id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM agent_mailbox_message inflight
      WHERE inflight.target_session_id = agent_mailbox_message.target_session_id
        AND inflight.id <> agent_mailbox_message.id
        AND inflight.delivery_status = 'leased'
        AND inflight.lease_expires_at > now()
  )
RETURNING *;

-- name: RecordDeliveryOutcome :one
-- One fenced write for every outcome. The token, daemon and unexpired lease
-- fence the update: a stale or foreign ack matches no row and cannot overwrite
-- a later lease. The retry schedule separates a busy host (short fixed delay)
-- from an unreachable host or a not-yet-confirmed outcome (bounded failure
-- backoff).
--
-- A withdrawn notification never returns to claimable delivery. A known
-- no-effect answer (pending) to a row withdrawn while it was leased becomes
-- failed and keeps the withdrawal reason; the host did not accept it, and the
-- producer already withdrew it. Delivered and unknown keep their own truthful
-- state: the marker cannot rewrite an accepted send, and an unconfirmed one is
-- only ever looked up by its original id.
UPDATE agent_mailbox_message
SET delivery_status = CASE
        WHEN @status = 'pending' AND withdrawn_at IS NOT NULL THEN 'failed'
        ELSE @status
    END,
    delivered_at = CASE WHEN @status = 'delivered' THEN now() ELSE delivered_at END,
    host_receipt = COALESCE(sqlc.narg('host_receipt'), host_receipt),
    last_error = CASE
        WHEN @status = 'pending' AND withdrawn_at IS NOT NULL
            THEN COALESCE(last_error, 'Notification withdrawn before it was accepted')
        ELSE sqlc.narg('last_error')
    END,
    next_attempt_at = CASE
        WHEN @status = 'pending' AND withdrawn_at IS NOT NULL THEN next_attempt_at
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

-- name: WithdrawAgentDeliveries :execrows
-- Withdraws the unaccepted notifications of archived agents. It runs in the
-- same transaction as the archive write, so an archive can never commit while
-- a notification it should have withdrawn stays claimable. Pending rows become
-- failed; leased and unknown rows keep their transport state (the host may
-- already have acted) and carry withdrawn_at so a known no-effect answer can
-- never return them to claimable delivery. target_session_id scopes it to the
-- delivery core: legacy internal-mailbox rows have no target and are consumed
-- through processing_status, so their delivery_status must stay untouched.
UPDATE agent_mailbox_message
SET delivery_status = CASE WHEN delivery_status = 'pending' THEN 'failed' ELSE delivery_status END,
    withdrawn_at = COALESCE(withdrawn_at, now()),
    -- A pending row's withdrawal is certain, so the reason is the reason. A
    -- leased or unknown row keeps any diagnostic it already had (the host may
    -- have said something useful); the marker still records the withdrawal.
    last_error = CASE WHEN delivery_status = 'pending' THEN @reason
                      ELSE COALESCE(NULLIF(last_error, ''), @reason) END,
    updated_at = now()
WHERE recipient_agent_id = ANY(@agent_ids::uuid[])
  AND target_session_id IS NOT NULL
  AND delivery_status IN ('pending','leased','unknown')
  AND withdrawn_at IS NULL;

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
-- mailbox API's keyset pagination. Only the legacy comment projection is
-- included: the frozen issue DTO has no way to render a generic delivery, and a
-- generic row may legitimately carry only an issue reference.
SELECT * FROM agent_mailbox_message
WHERE issue_id = @issue_id AND workspace_id = @workspace_id
  AND source_type = 'comment'
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
