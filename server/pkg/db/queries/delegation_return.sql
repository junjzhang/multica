-- name: CreateIssueDelegationReturn :one
-- The accepted return is written with the dispatching business write in one
-- transaction. A concurrent dispatch by the SAME actor loses on
-- uq_issue_delegation_return_dispatcher and re-reads the winner instead of
-- replacing it; a different actor has its own key and its own row.
INSERT INTO issue_delegation_return (
    id, workspace_id, issue_id, dispatcher_agent_id, return_kind,
    external_session_id, source_task_id
) VALUES (
    @id, @workspace_id, @issue_id, @dispatcher_agent_id, @return_kind,
    sqlc.narg('external_session_id'), sqlc.narg('source_task_id')
)
ON CONFLICT (workspace_id, issue_id, dispatcher_agent_id) DO NOTHING
RETURNING *;

-- name: GetIssueDelegationReturnForDispatcher :one
-- One actor's accepted return on one issue. The caller must pass the actor it
-- is dispatching for; an issue-wide lookup would let a later dispatcher reuse
-- an earlier actor's address.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND dispatcher_agent_id = @dispatcher_agent_id;

-- name: ListIssueDelegationReturns :many
-- Every accepted return relationship recorded on the issue, oldest first.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
ORDER BY created_at, id;

-- name: ListUnlinkedIssueDelegationReturns :many
-- Accepted relationships that no run carries yet: a backlog delegation, or a
-- return recorded for work whose run was never started. Starting a run must
-- attach these rather than guess which one applies.
SELECT r.*
FROM issue_delegation_return r
WHERE r.workspace_id = @workspace_id
  AND r.issue_id = @issue_id
  AND NOT EXISTS (
      SELECT 1 FROM agent_task_delegation_return l
      WHERE l.delegation_return_id = r.id
  )
ORDER BY r.created_at, r.id;

-- name: ListAgentTaskDelegationReturns :many
-- The complete set of accepted return relationships one run carries. Result
-- submission reads this set so a coalesced run notifies every dispatcher.
SELECT r.*
FROM issue_delegation_return r
JOIN agent_task_delegation_return l ON l.delegation_return_id = r.id
WHERE l.task_id = @task_id
ORDER BY r.created_at, r.id;

-- name: LinkAgentTaskDelegationReturn :execrows
-- Adds one accepted relationship to a run. The pair is idempotent, so a rerun,
-- a folded comment, or a retry can link the same relationship again without
-- rewriting the run's existing set.
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id)
VALUES (@task_id, @delegation_return_id)
ON CONFLICT (task_id, delegation_return_id) DO NOTHING;

-- name: LinkCommentTasksToDelegationReturn :execrows
-- Attaches the accepted relationship of one comment to every task that
-- comment became part of: the run it triggered, or the pending run it was
-- folded into (its id lands in coalesced_comment_ids). A run it merely steered
-- is also covered, and linking is idempotent.
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id)
SELECT t.id, @delegation_return_id
FROM agent_task_queue t
WHERE t.issue_id = @issue_id
  AND (
      t.trigger_comment_id = @comment_id
      OR @comment_id = ANY(t.coalesced_comment_ids)
  )
ON CONFLICT (task_id, delegation_return_id) DO NOTHING;

-- name: GetLatestTaskIDForIssueAndAgent :one
-- The most recent earlier run for one issue/executor pair. Rerun and executor
-- reassignment copy that run's accepted relationships instead of substituting
-- an arbitrary issue-level row.
SELECT id FROM agent_task_queue
WHERE issue_id = @issue_id
  AND agent_id = @agent_id
  AND (
      sqlc.narg('exclude_task_id')::uuid IS NULL
      OR id <> sqlc.narg('exclude_task_id')
  )
ORDER BY created_at DESC
LIMIT 1;
