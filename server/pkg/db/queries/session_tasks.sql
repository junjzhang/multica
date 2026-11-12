-- Read projections for the Paseo session task panel. Each read is scoped to
-- one workspace and one agent/session; callers authorize the credential before
-- these run. Limits keep a single panel read bounded.

-- name: ListSessionOwnedIssues :many
-- Issues this agent owns as executor (assignee) or as task leader of the
-- leadership root. Custom statuses are filtered by their effective behavior so
-- a workspace-defined done status does not leave finished work on the panel.
SELECT i.id, i.number, i.title, i.status, i.updated_at,
       (SELECT count(*)::int FROM issue child WHERE child.workspace_id = i.workspace_id AND child.parent_issue_id = i.id) AS child_count,
       (SELECT count(*)::int FROM issue child WHERE child.workspace_id = i.workspace_id AND child.parent_issue_id = i.id AND issue_effective_status(child.workspace_id, child.status) IN ('done', 'cancelled')) AS closed_child_count,
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
-- issue contributes at most one item. Only actual, unresolved run obligations
-- count; an accepted address alone does not imply an outstanding result.
SELECT r.id AS delegation_return_id, r.issue_id, r.created_at AS accepted_at,
       i.number, i.title, i.status AS issue_status, i.updated_at AS issue_updated_at,
       COALESCE(a.name, '')::text AS executor_name,
       COALESCE(latest.started_at, r.created_at)::timestamptz AS activity_at,
       COALESCE(latest.task_status, '')::text AS task_status
FROM issue_delegation_return r
JOIN issue i ON i.id = r.issue_id AND i.workspace_id = r.workspace_id
JOIN LATERAL (
    SELECT t.status AS task_status, t.agent_id, t.started_at
    FROM agent_task_delegation_return l
    JOIN agent_task_queue t ON t.id = l.task_id
    WHERE l.delegation_return_id = r.id
      AND l.materialized_at IS NULL
      AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred', 'completed', 'failed')
      AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = t.id)
      AND NOT EXISTS (
          SELECT 1 FROM delegation_result_takeover takeover
          WHERE takeover.result_task_id = t.id AND takeover.from_return_id = r.id
      )
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT 1
) latest ON true
LEFT JOIN agent a ON a.id = latest.agent_id AND a.workspace_id = i.workspace_id
WHERE r.workspace_id = @workspace_id
  AND r.dispatcher_agent_id = @agent_id
  AND r.external_session_id = @external_session_id
  AND r.is_current
  AND issue_effective_status(i.workspace_id, i.status) NOT IN ('done', 'cancelled')
ORDER BY r.created_at DESC, r.id
LIMIT sqlc.arg('row_limit')::int;

-- name: ListSessionDelegatedIssues :many
-- Current accepted dispatch addresses with at least one run, including runs
-- whose result was already returned while the issue remains open.
SELECT r.issue_id, i.number, i.title, i.status AS issue_status, i.updated_at AS issue_updated_at,
       COALESCE(a.name, '')::text AS executor_name,
       COALESCE(latest.started_at, latest.created_at)::timestamptz AS activity_at,
       latest.task_status, c.data AS contract_data,
       COALESCE((SELECT split_part(cm.content, E'\n', 1) FROM comment cm
         WHERE cm.workspace_id = i.workspace_id AND cm.issue_id = i.id
           AND cm.author_type = i.assignee_type AND cm.author_id = i.assignee_id
           AND cm.type = 'comment' AND cm.deleted_at IS NULL
         ORDER BY cm.created_at DESC, cm.id DESC LIMIT 1), '')::text AS blocked_reason
FROM issue_delegation_return r
JOIN issue i ON i.id = r.issue_id AND i.workspace_id = r.workspace_id
JOIN LATERAL (
    SELECT t.status AS task_status, t.agent_id, t.started_at, t.created_at
    FROM agent_task_delegation_return l
    JOIN agent_task_queue t ON t.id = l.task_id
    WHERE l.delegation_return_id = r.id
      AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = t.id)
      AND NOT EXISTS (SELECT 1 FROM delegation_result_takeover takeover WHERE takeover.result_task_id = t.id AND takeover.from_return_id = r.id)
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT 1
) latest ON true
LEFT JOIN agent a ON a.id = latest.agent_id AND a.workspace_id = i.workspace_id
LEFT JOIN issue_contract c ON c.issue_id = i.id AND c.workspace_id = i.workspace_id
WHERE r.workspace_id = @workspace_id AND r.dispatcher_agent_id = @agent_id
  AND r.external_session_id = @external_session_id AND r.is_current
  AND issue_effective_status(i.workspace_id, i.status) NOT IN ('done', 'cancelled')
ORDER BY i.updated_at DESC, i.id
LIMIT sqlc.arg('row_limit')::int;

-- name: ListSessionUnansweredRequests :many
-- A request is a current executor's explicit mention, with no later comment
-- from the recipient in the same thread. Mailbox receipts do not imply reply.
SELECT c.id AS comment_id, c.content, c.created_at, i.id AS issue_id,
       i.number, i.title, i.status, i.updated_at
FROM comment c
JOIN issue i ON i.id = c.issue_id AND i.workspace_id = c.workspace_id
WHERE i.workspace_id = @workspace_id
  AND issue_effective_status(i.workspace_id, i.status) NOT IN ('done', 'cancelled')
  AND c.author_type = i.assignee_type AND c.author_id = i.assignee_id
  AND c.type IN ('comment', 'progress_update') AND c.deleted_at IS NULL
  AND lower(c.content) LIKE ('%mention://agent/' || lower(sqlc.arg('agent_id')::text) || ')%')
  AND c.content ~* ('\[@(?:(?!\]\()[^[:cntrl:]])+\]\(mention://agent/' || sqlc.arg('agent_id')::text || '\)')
  AND NOT EXISTS (SELECT 1 FROM comment root WHERE root.id = comment_thread_root_id(c.id) AND root.resolved_at IS NOT NULL)
  AND NOT EXISTS (
    SELECT 1 FROM comment reply
    WHERE reply.workspace_id = c.workspace_id AND reply.issue_id = c.issue_id
      AND reply.author_type = 'agent' AND reply.author_id = sqlc.arg('agent_id')::uuid
      AND reply.type IN ('comment', 'progress_update') AND reply.deleted_at IS NULL
      AND (reply.created_at, reply.id) > (c.created_at, c.id)
      AND comment_thread_root_id(reply.id) = comment_thread_root_id(c.id)
  )
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg('row_limit')::int;
