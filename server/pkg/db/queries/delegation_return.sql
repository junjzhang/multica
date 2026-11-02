-- name: CreateIssueDelegationReturn :one
-- The accepted return is written with the dispatching business write in one
-- transaction. A concurrent dispatch for the same (issue, dispatcher) loses on
-- uq_issue_delegation_return_dispatch and re-reads the winner instead of
-- replacing it; another dispatcher has its own key and row.
INSERT INTO issue_delegation_return (
    id, workspace_id, issue_id, dispatcher_agent_id,
    return_kind, external_session_id, source_task_id
) VALUES (
    @id, @workspace_id, @issue_id, @dispatcher_agent_id,
    @return_kind, sqlc.narg('external_session_id'), sqlc.narg('source_task_id')
)
ON CONFLICT (workspace_id, issue_id, dispatcher_agent_id) DO NOTHING
RETURNING *;

-- name: GetIssueDelegationReturnForDispatcher :one
-- One actor's accepted address on one issue, independent of the assignment
-- slot it dispatched to.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND dispatcher_agent_id = @dispatcher_agent_id;

-- name: LockIssueDelegationReturnForDispatcher :one
-- The accepted row is locked while a dispatch decides whether it is reusable.
-- A concurrent takeover or first dispatch therefore serializes before the row
-- is read or updated.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND dispatcher_agent_id = @dispatcher_agent_id
FOR UPDATE;

-- name: UpdateIssueDelegationReturnExternalSession :one
-- The explicit external takeover endpoint changed the (agent, issue) binding;
-- the accepted address follows that binding. This is the only writer allowed
-- to replace an accepted external address.
UPDATE issue_delegation_return
SET external_session_id = @external_session_id
WHERE id = @id AND return_kind = 'external_session'
RETURNING *;

-- name: ListIssueDelegationReturns :many
-- Every accepted return relationship recorded on the issue, oldest first.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
ORDER BY created_at, id;

-- name: LinkCommentDelegationReturn :execrows
-- Records the accepted relation between one comment intent and one dispatcher's
-- address. The pair is idempotent so an edit or replay can add it again.
INSERT INTO comment_delegation_return (comment_id, delegation_return_id)
VALUES (@comment_id, @delegation_return_id)
ON CONFLICT (comment_id, delegation_return_id) DO NOTHING;

-- name: ListCommentDelegationReturnIDs :many
-- The accepted relationships a comment carries. New runs, folds, completion
-- replay and edit/cancel retrigger read this instead of an in-memory set.
SELECT delegation_return_id FROM comment_delegation_return
WHERE comment_id = @comment_id
ORDER BY created_at, delegation_return_id;

-- name: DeleteCommentDelegationReturns :exec
-- A deleted comment is no longer a dispatch intent. Clean its relations in the
-- same transaction that tombstones or removes it.
DELETE FROM comment_delegation_return WHERE comment_id = @comment_id;

-- name: UpsertIssueAssignmentDelegation :one
-- The current intent for one assignment slot. Starting a backlog slot updates
-- the same header with the task that finally carries it instead of creating a
-- second intent.
INSERT INTO issue_assignment_delegation (issue_id, workspace_id, assignee_type, assignee_id, task_id)
VALUES (@issue_id, @workspace_id, @assignee_type, @assignee_id, sqlc.narg('task_id'))
ON CONFLICT (issue_id, assignee_type, assignee_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id,
    task_id = EXCLUDED.task_id,
    created_at = now()
RETURNING *;

-- name: GetIssueAssignmentDelegation :one
SELECT * FROM issue_assignment_delegation
WHERE issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id;

-- name: SetIssueAssignmentDelegationTask :exec
UPDATE issue_assignment_delegation
SET task_id = @task_id
WHERE issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id;

-- name: DeleteIssueAssignmentDelegation :exec
DELETE FROM issue_assignment_delegation
WHERE issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id;

-- name: DeleteIssueAssignmentDelegationReturns :exec
DELETE FROM issue_assignment_delegation_return
WHERE issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id;

-- name: LinkIssueAssignmentDelegationReturn :execrows
INSERT INTO issue_assignment_delegation_return (
    issue_id, assignee_type, assignee_id, delegation_return_id
) VALUES (@issue_id, @assignee_type, @assignee_id, @delegation_return_id)
ON CONFLICT (issue_id, assignee_type, assignee_id, delegation_return_id) DO NOTHING;

-- name: ListIssueAssignmentDelegationReturnIDs :many
SELECT delegation_return_id FROM issue_assignment_delegation_return
WHERE issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id
ORDER BY created_at, delegation_return_id;

-- name: GetPendingAssignmentTaskForIssueAndAgent :one
-- The run an assignment coalesces into when its own insert loses the pending
-- slot. Assignment runs have no comment thread, so this never picks up a
-- comment-triggered run from another thread.
SELECT * FROM agent_task_queue
WHERE issue_id = @issue_id
  AND agent_id = @agent_id
  AND comment_thread_id IS NULL
  AND status IN ('queued', 'dispatched')
ORDER BY created_at DESC
LIMIT 1;

-- name: GetLatestTaskIDForIssueAndAgent :one
-- The most recent earlier run for one issue/executor pair. Rerun uses it to
-- find the source run whose exact accepted relationships it continues.
SELECT id FROM agent_task_queue
WHERE issue_id = @issue_id
  AND agent_id = @agent_id
  AND (
      sqlc.narg('exclude_task_id')::uuid IS NULL
      OR id <> sqlc.narg('exclude_task_id')
  )
ORDER BY created_at DESC
LIMIT 1;

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
