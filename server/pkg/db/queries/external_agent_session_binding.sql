-- name: CreateAgentSessionBinding :one
-- One active binding per exact (workspace, agent, issue) key. A replacement
-- revokes the previous row first in the same transaction, so readers never
-- see two active targets. The unique partial index is the last word under
-- concurrency.
INSERT INTO external_agent_session_binding (
    workspace_id, agent_id, issue_id, session_id, created_by
) VALUES (
    @workspace_id, @agent_id, @issue_id, @session_id, sqlc.narg('created_by')
)
RETURNING *;

-- name: GetActiveAgentSessionBinding :one
-- Resolves the one authorized target for this agent on this issue before a
-- producer enqueues. FOR SHARE serializes with a concurrent rebind: the
-- enqueue that follows commits or the revoke commits, never both.
--
-- There is deliberately no fallback to another session of the same agent: an
-- exact key with no active binding is unaddressed and the producer must fail
-- closed. Archived agents and sessions whose owner left the workspace resolve
-- nothing, so an enqueue cannot outrun an immediate constraint either.
SELECT b.id,
       b.workspace_id,
       b.agent_id,
       b.issue_id,
       b.session_id,
       s.provider,
       s.host_id,
       s.session_id AS host_session_id,
       s.daemon_id,
       s.owner_user_id
FROM external_agent_session_binding b
JOIN external_agent_session s
  ON s.id = b.session_id
 AND s.workspace_id = b.workspace_id
 AND s.agent_id = b.agent_id
JOIN agent a
  ON a.id = s.agent_id
 AND a.workspace_id = b.workspace_id
JOIN member m
  ON m.workspace_id = b.workspace_id
 AND m.user_id = s.owner_user_id
WHERE b.workspace_id = @workspace_id
  AND b.agent_id = @agent_id
  AND b.issue_id = @issue_id
  AND b.revoked_at IS NULL
  AND a.execution_kind = 'external'
  AND a.archived_at IS NULL
FOR SHARE OF b;

-- name: RevokeActiveAgentSessionBindings :execrows
-- Explicit takeover: future notifications resolve the replacement, earlier
-- delivery rows keep the target they froze.
UPDATE external_agent_session_binding
SET revoked_at = now()
WHERE workspace_id = @workspace_id
  AND agent_id = @agent_id
  AND issue_id = @issue_id
  AND revoked_at IS NULL;

-- name: DeleteAgentSessionBindingsForIssue :exec
DELETE FROM external_agent_session_binding WHERE issue_id = $1;

-- name: DeleteAgentSessionBindingsForWorkspace :exec
DELETE FROM external_agent_session_binding WHERE workspace_id = $1;

-- name: DeleteAgentSessionBindingsForSession :exec
DELETE FROM external_agent_session_binding WHERE session_id = $1;
