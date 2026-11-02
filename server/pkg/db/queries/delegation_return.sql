-- name: CreateIssueDelegationReturn :one
-- The accepted return is written with the dispatching business write in one
-- transaction. A concurrent dispatch for the SAME (dispatcher, assignee) key
-- loses on uq_issue_delegation_return_dispatch and re-reads the winner instead
-- of replacing it; a different dispatcher or assignee has its own key and row.
INSERT INTO issue_delegation_return (
    id, workspace_id, issue_id, dispatcher_agent_id, assignee_type, assignee_id,
    return_kind, external_session_id, source_task_id
) VALUES (
    @id, @workspace_id, @issue_id, @dispatcher_agent_id, @assignee_type, @assignee_id,
    @return_kind, sqlc.narg('external_session_id'), sqlc.narg('source_task_id')
)
ON CONFLICT (workspace_id, issue_id, dispatcher_agent_id, assignee_type, assignee_id) DO NOTHING
RETURNING *;

-- name: GetIssueDelegationReturnForDispatch :one
-- One actor's accepted return for one exact assignment slot. The caller must
-- pass the target it is dispatching to; an issue-wide lookup would let a later
-- dispatcher reuse an earlier actor's address.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND dispatcher_agent_id = @dispatcher_agent_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id;

-- name: ListIssueDelegationReturns :many
-- Every accepted return relationship recorded on the issue, oldest first.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
ORDER BY created_at, id;

-- name: ListIssueDelegationReturnsForAssignee :many
-- The accepted relationships recorded for one assignment slot. A run starting
-- for this assignee carries exactly this set plus its source run's inherited
-- set; it never guesses from every return on the issue, and a backlog row stays
-- available even after another assignee's run already exists.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id
ORDER BY created_at, id;

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

-- name: LockAgentTaskForDelegation :one
-- Re-reads the dispatching managed run under a share lock inside the
-- transaction that persists a new delegation. A run that completed (or was
-- cancelled/failed) between authentication and this write can no longer accept
-- new child work, and a pin removed in that window is not an address. A
-- completed row remains a valid target for relationships it already owns; this
-- lock only decides whether a NEW delegation may be accepted.
SELECT * FROM agent_task_queue WHERE id = @id FOR SHARE;

-- name: LockAgentForDelegation :one
-- Re-checks the dispatching agent's current authorization under a share lock:
-- archived, no longer managed, or moved to another runtime all invalidate the
-- address between authentication and the write.
SELECT * FROM agent WHERE id = @id AND workspace_id = @workspace_id FOR SHARE;
