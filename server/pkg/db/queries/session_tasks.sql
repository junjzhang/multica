-- Read projections for the Paseo session task panel. Each read is scoped to
-- one workspace and one agent/session; callers authorize the credential before
-- these run. Limits keep a single panel read bounded.

-- name: ListSessionOwnedIssues :many
-- Issues this agent owns as executor (assignee) or as task leader of the
-- leadership root. Custom statuses are filtered by their effective behavior so
-- a workspace-defined done status does not leave finished work on the panel.
SELECT i.id, i.number, i.title, i.status, i.updated_at,
       COALESCE(i.assignee_type = 'agent' AND i.assignee_id = @agent_id, false)::boolean AS is_executor
FROM issue i
LEFT JOIN issue r ON r.id = COALESCE(i.leadership_root_issue_id, i.id) AND r.workspace_id = i.workspace_id
WHERE i.workspace_id = @workspace_id
  AND (
    (i.assignee_type = 'agent' AND i.assignee_id = @agent_id)
    OR r.leader_agent_id = @agent_id
  )
  AND issue_effective_status(i.workspace_id, i.status) NOT IN ('done', 'cancelled')
ORDER BY i.updated_at DESC, i.id
LIMIT sqlc.arg('row_limit')::int;

-- name: ListSessionAcceptingContractIssues :many
-- Contracts whose current acceptor may be this agent. The JSON filter is a
-- superset check (any version names the agent); the handler keeps only the
-- current version and only while a delivery is pending, so a finished
-- acceptance drops out without a second query shape.
SELECT i.id, i.number, i.title, i.status, i.updated_at, c.data
FROM issue i
JOIN issue_contract c ON c.issue_id = i.id AND c.workspace_id = i.workspace_id
WHERE i.workspace_id = @workspace_id
  AND issue_effective_status(i.workspace_id, i.status) NOT IN ('done', 'cancelled')
  AND c.data->'versions' @> jsonb_build_array(
        jsonb_build_object('acceptor', jsonb_build_object('id', sqlc.arg('agent_id')::text)))
ORDER BY i.updated_at DESC, i.id
LIMIT sqlc.arg('row_limit')::int;

-- name: ListSessionPendingDelegationReturns :many
-- Accepted delegation returns this session dispatched and is still waiting on.
-- The return row is the dispatcher's accepted address for one issue, so each
-- issue contributes at most one item. A delegation is "returned" once the
-- latest run carrying that address has materialized its result; a retry or a
-- follow-up dispatch makes it pending again.
SELECT r.id AS delegation_return_id, r.issue_id, r.created_at AS accepted_at,
       i.number, i.title, i.status AS issue_status, i.updated_at AS issue_updated_at,
       COALESCE(latest.task_status, '')::text AS task_status
FROM issue_delegation_return r
JOIN issue i ON i.id = r.issue_id AND i.workspace_id = r.workspace_id
LEFT JOIN LATERAL (
    SELECT t.status AS task_status, l.materialized_at
    FROM agent_task_delegation_return l
    JOIN agent_task_queue t ON t.id = l.task_id
    WHERE l.delegation_return_id = r.id
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT 1
) latest ON true
WHERE r.workspace_id = @workspace_id
  AND r.dispatcher_agent_id = @agent_id
  AND r.external_session_id = @external_session_id
  AND r.is_current
  AND issue_effective_status(i.workspace_id, i.status) <> 'cancelled'
  AND (latest.task_status IS NULL OR latest.materialized_at IS NULL)
ORDER BY r.created_at DESC, r.id
LIMIT sqlc.arg('row_limit')::int;
