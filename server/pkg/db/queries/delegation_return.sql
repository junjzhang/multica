-- name: CreateIssueDelegationReturn :one
-- The accepted return is written with the delegated issue (or with the first
-- run dispatched for it) in one transaction. A concurrent dispatch for the same
-- issue loses on uq_issue_delegation_return_issue and re-reads the winner
-- instead of replacing it; see GetIssueDelegationReturn.
INSERT INTO issue_delegation_return (
    id, workspace_id, issue_id, dispatcher_agent_id, return_kind,
    external_session_id, source_task_id
) VALUES (
    @id, @workspace_id, @issue_id, @dispatcher_agent_id, @return_kind,
    sqlc.narg('external_session_id'), sqlc.narg('source_task_id')
)
ON CONFLICT (workspace_id, issue_id) DO NOTHING
RETURNING *;

-- name: GetIssueDelegationReturn :one
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id AND issue_id = @issue_id;

-- name: SetAgentTaskDelegationReturn :execrows
-- Links one dispatched run to the accepted return. It is written after the
-- task insert in the same transaction and never rewrites an existing link, so
-- a rerun or a folded comment keeps the original recipient.
UPDATE agent_task_queue
SET delegation_return_id = @delegation_return_id
WHERE id = @id AND delegation_return_id IS NULL;

-- name: GetIssueDelegationReturnForTask :one
SELECT r.*
FROM issue_delegation_return r
JOIN agent_task_queue t ON t.delegation_return_id = r.id
WHERE t.id = @task_id;

-- name: LinkTriggeredTasksToDelegationReturn :execrows
-- A comment that starts a fresh run records which accepted delegation return
-- that run belongs to. Only tasks this exact comment triggered are touched:
-- a run the comment was folded into keeps its original link and recipient.
UPDATE agent_task_queue
SET delegation_return_id = @delegation_return_id
WHERE issue_id = @issue_id
  AND trigger_comment_id = @trigger_comment_id
  AND delegation_return_id IS NULL;
