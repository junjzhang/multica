-- Archive safety for an external agent. The preflight and the archive write
-- share these queries so the preview a human approved is the same set the
-- write re-checks under a lock.

-- name: LockAgentForArchive :one
-- FOR UPDATE serializes the archive with assignment/dispatch paths that take
-- the same row (LockAgentForAutopilotAssignment uses FOR SHARE), so a new
-- reference either lands before the final check or is refused after the
-- archive commits.
SELECT * FROM agent
WHERE id = @id AND workspace_id = @workspace_id AND kind = 'user'
FOR UPDATE;

-- name: ListAgentArchiveBlockers :many
-- Every in-flight reference that must be resolved before an agent can be
-- archived. One row per reference; the handler groups them per code and keeps
-- a bounded sample beside the true count.
WITH refs AS (
    SELECT 'in_flight_tasks'::text AS code, t.id::text AS ref_id
    FROM agent_task_queue t
    WHERE t.agent_id = @agent_id
      AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
    UNION ALL
    SELECT 'active_waits', t.id::text
    FROM agent_task_queue t
    WHERE t.agent_id = @agent_id
      AND t.context ? 'background_wait'
      AND t.context->'background_wait'->>'consumed' IS DISTINCT FROM 'true'
    UNION ALL
    SELECT 'pending_messages', m.id::text
    FROM agent_mailbox_message m
    WHERE m.recipient_agent_id = @agent_id
      AND m.delivery_status = 'pending'
      AND m.withdrawn_at IS NULL
    UNION ALL
    SELECT 'unverified_messages', m.id::text
    FROM agent_mailbox_message m
    WHERE m.recipient_agent_id = @agent_id
      AND m.delivery_status IN ('leased', 'unknown')
      AND m.withdrawn_at IS NULL
    UNION ALL
    SELECT 'current_returns', r.id::text
    FROM issue_delegation_return r
    WHERE r.dispatcher_agent_id = @agent_id
      AND r.is_current
    UNION ALL
    SELECT 'squad_default_leader', s.id::text
    FROM squad s
    WHERE s.leader_id = @agent_id AND s.archived_at IS NULL
    UNION ALL
    SELECT 'squad_memberships', sm.id::text
    FROM squad_member sm
    WHERE sm.member_type = 'agent' AND sm.member_id = @agent_id
    UNION ALL
    SELECT 'assigned_issues', i.id::text
    FROM issue i
    WHERE i.assignee_type = 'agent' AND i.assignee_id = @agent_id
      AND i.status NOT IN ('done', 'cancelled')
    UNION ALL
    SELECT 'task_leaderships', i.id::text
    FROM issue i
    WHERE i.leader_agent_id = @agent_id
      AND i.status NOT IN ('done', 'cancelled')
    UNION ALL
    SELECT 'coordinated_issues', i.id::text
    FROM issue i
    WHERE i.coordinator_type = 'agent' AND i.coordinator_id = @agent_id
      AND i.status NOT IN ('done', 'cancelled')
)
SELECT code, ref_id
FROM refs
ORDER BY code, ref_id;

-- name: ListAgentOpenContractCandidates :many
-- Contracts are stored as one JSON state per issue, so a text prefilter narrows
-- the rows to the agent's id and the decision is made on the parsed state.
SELECT c.issue_id, c.data
FROM issue_contract c
WHERE c.workspace_id = @workspace_id
  AND c.data::text LIKE '%' || @agent_id::text || '%';

-- name: RevokeExternalAgentTokens :execrows
-- Archive revokes every live credential in the same transaction. Session rows
-- keep their identity for audit; the archived agent makes them undeliverable.
UPDATE external_agent_token
SET revoked_at = now()
WHERE agent_id = @agent_id
  AND workspace_id = @workspace_id
  AND revoked_at IS NULL;
