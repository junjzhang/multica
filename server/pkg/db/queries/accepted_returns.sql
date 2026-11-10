-- name: ListAcceptedTaskReturns :many
SELECT l.task_id, r.id AS return_id, r.dispatcher_agent_id, r.return_kind,
       r.source_task_id, r.external_session_id
FROM agent_task_delegation_return l
JOIN issue_delegation_return r ON r.id = l.delegation_return_id
WHERE l.task_id = ANY(@task_ids::uuid[]) AND r.workspace_id = @workspace_id
  AND l.takeover_id IS NULL
ORDER BY l.task_id, r.created_at, r.id;
