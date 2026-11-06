-- name: CreateIssueDelegationReturn :one
-- The accepted return is written with the dispatching business write in one
-- transaction. A concurrent dispatch for the same (issue, dispatcher) loses on
-- uq_current_issue_delegation_return and re-reads the winner instead of
-- replacing it; another dispatcher has its own key and row.
INSERT INTO issue_delegation_return (
    id, workspace_id, issue_id, dispatcher_agent_id,
    return_kind, external_session_id, source_task_id,
    runtime_id, provider, session_id, work_dir, scope_task_id
) VALUES (
    @id, @workspace_id, @issue_id, @dispatcher_agent_id,
    @return_kind, sqlc.narg('external_session_id'), sqlc.narg('source_task_id'),
    sqlc.narg('runtime_id'), sqlc.narg('provider'), sqlc.narg('session_id'), sqlc.narg('work_dir'), sqlc.narg('scope_task_id')
)
ON CONFLICT (workspace_id, issue_id, dispatcher_agent_id) WHERE is_current DO NOTHING
RETURNING *;

-- name: GetIssueDelegationReturnForDispatcher :one
-- One actor's current accepted version on one issue, independent of the assignment
-- slot it dispatched to.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND dispatcher_agent_id = @dispatcher_agent_id AND is_current;

-- name: LockIssueDelegationReturnForDispatcher :one
-- The accepted row is locked while a dispatch decides whether it is reusable.
-- A concurrent takeover or first dispatch therefore serializes before the row
-- is read or updated.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id
  AND issue_id = @issue_id
  AND dispatcher_agent_id = @dispatcher_agent_id AND is_current
FOR UPDATE;

-- name: ListIssueDelegationReturns :many
-- Every accepted return relationship recorded on the issue, oldest first.
SELECT * FROM issue_delegation_return
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
ORDER BY created_at, id;

-- name: LinkCommentDelegationReturn :execrows
-- Records the accepted relation between one comment intent and one dispatcher's
-- address. The pair is idempotent so an edit or replay can add it again.
INSERT INTO comment_delegation_return (comment_id, delegation_return_id, caller_task_ids)
VALUES (@comment_id, @delegation_return_id, sqlc.narg(caller_task_ids)::uuid[])
ON CONFLICT (comment_id, delegation_return_id) DO NOTHING;

-- name: ListCommentDelegationReturnIntents :many
-- The accepted relationships a comment carries. New runs, folds, completion
-- replay and edit/cancel retrigger read this instead of an in-memory set.
SELECT delegation_return_id, caller_task_ids FROM comment_delegation_return
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
    issue_id, assignee_type, assignee_id, delegation_return_id, caller_task_ids
) VALUES (@issue_id, @assignee_type, @assignee_id, @delegation_return_id, sqlc.narg(caller_task_ids)::uuid[])
ON CONFLICT (issue_id, assignee_type, assignee_id, delegation_return_id) DO NOTHING;

-- name: ListIssueAssignmentDelegationReturnIntents :many
SELECT delegation_return_id, caller_task_ids FROM issue_assignment_delegation_return
WHERE issue_id = @issue_id
  AND assignee_type = @assignee_type
  AND assignee_id = @assignee_id
ORDER BY created_at, delegation_return_id;

-- name: GetPendingAssignmentTaskForIssueAndAgent :one
-- The run an assignment coalesces into when its own insert loses the pending
-- slot. The predicate must match idx_one_pending_task_per_issue_agent_thread
-- exactly, including the channel-media deferred exception; a narrower read
-- turns an existing occupant into a silent no-run success. Assignment runs
-- have no comment thread, so this never picks up a comment-triggered run from
-- another thread.
SELECT * FROM agent_task_queue
WHERE issue_id = @issue_id
  AND COALESCE(context->>'type', '') <> 'delegation_result'
  AND agent_id = @agent_id
  AND comment_thread_id IS NULL
  AND (
      status IN ('queued', 'dispatched')
      OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
ORDER BY created_at DESC
LIMIT 1;

-- name: GetLatestTaskIDForIssueAndAgent :one
-- The most recent earlier run for one issue/executor pair. Rerun uses it to
-- find the source run whose exact accepted relationships it continues.
SELECT id FROM agent_task_queue
WHERE issue_id = @issue_id
  AND COALESCE(context->>'type', '') <> 'delegation_result'
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
WHERE l.task_id = @task_id AND l.takeover_id IS NULL
ORDER BY r.created_at, r.id;

-- name: ListRunReturnIntents :many
SELECT l.delegation_return_id, l.caller_task_ids, r.dispatcher_agent_id
FROM agent_task_delegation_return l
JOIN issue_delegation_return r ON r.id = l.delegation_return_id
WHERE l.task_id = @task_id AND l.takeover_id IS NULL
ORDER BY r.created_at, r.id;

-- name: LinkAgentTaskDelegationReturn :execrows
-- Adds one accepted relationship to a run. The pair is idempotent, so a rerun,
-- a folded comment, or a retry can link the same relationship again without
-- rewriting the run's existing set.
WITH RECURSIVE execution_chain AS (
    SELECT @task_id::uuid AS id
    UNION
    SELECT child.id FROM agent_task_queue child
    JOIN execution_chain parent ON child.retry_of_task_id = parent.id
)
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, caller_task_ids)
SELECT id, @delegation_return_id, sqlc.narg(caller_task_ids)::uuid[] FROM execution_chain
ON CONFLICT (task_id, delegation_return_id) DO UPDATE SET caller_task_ids =
CASE WHEN agent_task_delegation_return.caller_task_ids IS NULL OR EXCLUDED.caller_task_ids IS NULL THEN NULL
ELSE ARRAY(SELECT DISTINCT id FROM unnest(agent_task_delegation_return.caller_task_ids || EXCLUDED.caller_task_ids) id ORDER BY id) END;

-- name: CopyRetryDelegationReturns :execrows
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, caller_task_ids)
SELECT @child_task_id, parent.delegation_return_id, parent.caller_task_ids
FROM agent_task_delegation_return parent WHERE parent.task_id = @parent_task_id AND parent.takeover_id IS NULL
ON CONFLICT (task_id, delegation_return_id) DO UPDATE SET caller_task_ids =
CASE WHEN agent_task_delegation_return.caller_task_ids IS NULL OR EXCLUDED.caller_task_ids IS NULL THEN NULL
ELSE ARRAY(SELECT DISTINCT id FROM unnest(agent_task_delegation_return.caller_task_ids || EXCLUDED.caller_task_ids) id ORDER BY id) END;

-- name: PropagateRetryDelegationReturns :execrows
-- Close a concurrent late-link/retry-creation race from the durable parent
-- relation. Traversal follows retry_of_task_id only, never a newer manual run.
WITH RECURSIVE missing AS (
    SELECT child.id AS task_id, parent.delegation_return_id, parent.caller_task_ids
    FROM agent_task_delegation_return parent
    JOIN agent_task_queue child ON child.retry_of_task_id = parent.task_id
    WHERE parent.takeover_id IS NULL AND NOT EXISTS (
        SELECT 1 FROM agent_task_delegation_return present
        WHERE present.task_id = child.id AND present.delegation_return_id = parent.delegation_return_id
          AND (present.caller_task_ids IS NULL OR (parent.caller_task_ids IS NOT NULL AND parent.caller_task_ids <@ present.caller_task_ids))
    )
    ORDER BY child.id, parent.delegation_return_id
    LIMIT @batch_size
), descendants AS (
    SELECT * FROM missing
    UNION
    SELECT child.id, parent.delegation_return_id, parent.caller_task_ids
    FROM descendants parent
    JOIN agent_task_queue child ON child.retry_of_task_id = parent.task_id
)
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, caller_task_ids)
SELECT task_id, delegation_return_id,
       CASE WHEN bool_or(caller_task_ids IS NULL) THEN NULL
            ELSE COALESCE(array_agg(DISTINCT caller) FILTER (WHERE caller IS NOT NULL), '{}') END
FROM descendants LEFT JOIN LATERAL unnest(caller_task_ids) caller ON true
GROUP BY task_id, delegation_return_id
ON CONFLICT (task_id, delegation_return_id) DO UPDATE SET caller_task_ids =
CASE WHEN agent_task_delegation_return.caller_task_ids IS NULL OR EXCLUDED.caller_task_ids IS NULL THEN NULL
ELSE ARRAY(SELECT DISTINCT id FROM unnest(agent_task_delegation_return.caller_task_ids || EXCLUDED.caller_task_ids) id ORDER BY id) END;

-- name: LockRuntimeForDelegation :one
-- Freeze the source runtime's provider while accepting its address. Admission
-- takes runtime, agent, then task locks, matching the claim delivery gate.
SELECT * FROM agent_runtime
WHERE id = @id AND workspace_id = @workspace_id
FOR SHARE;

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
-- name: InsertCommentDispatchObligation :one
INSERT INTO comment_dispatch_obligation (
    id, workspace_id, issue_id, comment_id, agent_id, squad_id,
    trigger_source, delegation_return_id
) VALUES (
    @id, @workspace_id, @issue_id, @comment_id, @agent_id, sqlc.narg('squad_id'),
    @trigger_source, sqlc.narg('delegation_return_id')
)
ON CONFLICT (comment_id, agent_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id,
    issue_id = EXCLUDED.issue_id,
    squad_id = EXCLUDED.squad_id,
    trigger_source = EXCLUDED.trigger_source,
    delegation_return_id = EXCLUDED.delegation_return_id,
    status = 'pending',
    attempts = 0,
    last_error = NULL,
    settled_task_id = NULL,
    next_attempt_at = now(),
    updated_at = now()
RETURNING *;

-- name: DeleteCommentDispatchObligations :exec
DELETE FROM comment_dispatch_obligation WHERE comment_id = @comment_id;

-- name: GetCommentDispatchObligation :one
SELECT * FROM comment_dispatch_obligation WHERE id = @id;

-- name: ListPendingCommentDispatchObligations :many
SELECT * FROM comment_dispatch_obligation
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at, created_at, id
LIMIT @batch_size;

-- name: LockCommentDispatchObligation :one
SELECT * FROM comment_dispatch_obligation
WHERE id = @id
FOR UPDATE;

-- name: SettleCommentDispatchObligation :exec
UPDATE comment_dispatch_obligation
SET status = 'settled', settled_task_id = sqlc.narg('task_id'), last_error = NULL, updated_at = now()
WHERE id = @id AND status IN ('pending', 'settled');

-- name: GetCommentDispatchObligationForTarget :one
SELECT * FROM comment_dispatch_obligation
WHERE comment_id = @comment_id AND agent_id = @agent_id;

-- name: ReopenCommentDispatchObligation :exec
UPDATE comment_dispatch_obligation SET status = 'pending', updated_at = now()
WHERE id = @id AND status = 'settled';

-- name: FailCommentDispatchObligation :exec
UPDATE comment_dispatch_obligation
SET status = 'failed', last_error = @last_error, updated_at = now()
WHERE id = @id AND status IN ('pending', 'settled');

-- name: RecordCommentDispatchObligationAttempt :exec
-- Leaves a transiently failed obligation pending with the backoff the consumer
-- computed. The comment and accepted relation are already durable; only the
-- covering run is missing.
UPDATE comment_dispatch_obligation
SET attempts = attempts + 1,
    last_error = @last_error,
    next_attempt_at = @next_attempt_at,
    updated_at = now()
WHERE id = @id AND status = 'pending';

-- name: GetNewestActiveTaskForIssueAgentThread :one
-- The active run in one comment thread. A comment newer than that run is
-- already inside completion reconcile's timestamp window, so the comment
-- transaction must not append it as a planned id (that would create a second
-- replay path); the durable obligation waits for reconcile or the sweeper. A
-- comment older than the task is the lost-race shape that must be registered as
-- a planned input or its completion replay cannot see it.
SELECT * FROM agent_task_queue
WHERE issue_id = @issue_id
  AND COALESCE(context->>'type', '') <> 'delegation_result'
  AND agent_id = @agent_id
  AND comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(@comment_id)
  AND (
      status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
      OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
ORDER BY created_at DESC
LIMIT 1;

-- name: HasConflictingPendingReturnCallers :one
-- The return row is locked by acceptance. Check work that could still fold
-- together, not completed historical generations of this physical address.
WITH pending AS (
    SELECT l.caller_task_ids FROM agent_task_delegation_return l
    JOIN agent_task_queue task ON task.id = l.task_id
    WHERE l.delegation_return_id = @return_id AND l.takeover_id IS NULL
      AND task.status NOT IN ('completed', 'failed', 'cancelled')
    UNION ALL
    SELECT l.caller_task_ids FROM issue_assignment_delegation_return l
    JOIN issue_assignment_delegation assignment USING (issue_id, assignee_type, assignee_id)
    WHERE l.delegation_return_id = @return_id AND assignment.task_id IS NULL
    UNION ALL
    SELECT l.caller_task_ids FROM comment_delegation_return l
    JOIN comment_dispatch_obligation obligation USING (comment_id)
    WHERE l.delegation_return_id = @return_id AND obligation.status = 'pending'
)
SELECT EXISTS (
    SELECT 1 FROM pending
    LEFT JOIN LATERAL unnest(pending.caller_task_ids) caller_id ON true
    LEFT JOIN agent_task_queue caller ON caller.id = caller_id
    WHERE pending.caller_task_ids IS NULL OR caller.id IS NULL
       OR caller.originator_user_id IS DISTINCT FROM sqlc.narg(originator_user_id)::uuid
) AS conflict;
