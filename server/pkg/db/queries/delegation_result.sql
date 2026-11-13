-- name: GetDelegationReturnByID :one
SELECT * FROM issue_delegation_return WHERE id = @id;

-- name: HasTaskDelegationReturns :one
WITH RECURSIVE execution AS (
    SELECT t.id, t.retry_of_task_id FROM agent_task_queue t WHERE t.id = @task_id
    UNION
    SELECT parent.id, parent.retry_of_task_id FROM agent_task_queue parent
    JOIN execution child ON child.retry_of_task_id = parent.id
)
SELECT EXISTS (
    SELECT 1 FROM agent_task_delegation_return l JOIN execution e ON e.id = l.task_id WHERE l.takeover_id IS NULL
) AS present;

-- name: SetTaskDelegationOutcomeFinal :exec
UPDATE agent_task_queue SET delegation_outcome_final = @is_final
WHERE id = @id AND status IN ('completed', 'failed');

-- name: LockTaskForRetryDecision :one
-- Owner fences precede the attempt lock, matching retry insertion and teardown.
SELECT t.* FROM agent_task_queue t
WHERE t.id = @id AND lock_task_owner_rows(t.agent_id, t.issue_id, t.runtime_id)
FOR UPDATE OF t;

-- name: GetAutomaticRetryChild :one
SELECT * FROM agent_task_queue WHERE retry_of_task_id = @parent_task_id
ORDER BY created_at, id LIMIT 1;

-- name: ListUnresolvedDelegationFailures :many
SELECT t.* FROM agent_task_queue t
WHERE t.status = 'failed' AND NOT t.delegation_outcome_final
  AND EXISTS (SELECT 1 FROM agent_task_delegation_return l WHERE l.task_id = t.id AND l.takeover_id IS NULL)
  AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = t.id)
ORDER BY (SELECT min(COALESCE(l.last_attempt_at, '-infinity'::timestamptz)) FROM agent_task_delegation_return l WHERE l.task_id=t.id), t.completed_at, t.id LIMIT @batch_size;

-- name: ListReadyDelegationResults :many
SELECT l.task_id, l.delegation_return_id
FROM agent_task_delegation_return l
JOIN agent_task_queue t ON t.id = l.task_id
WHERE l.notification_task_id IS NULL AND l.notification_delivery_id IS NULL
  AND l.notification_error IS NULL
  AND (t.status = 'completed' OR (t.status = 'failed' AND t.delegation_outcome_final))
  AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = t.id)
ORDER BY l.last_attempt_at NULLS FIRST, t.completed_at, l.task_id, l.delegation_return_id
LIMIT @batch_size;

-- name: LockDelegationResultIssues :many
-- The outcome owner and original issue scope are locked in one stable order;
-- reciprocal delegations must not acquire each other's issues in reverse.
SELECT * FROM issue WHERE workspace_id = @workspace_id AND id = ANY(@issue_ids::uuid[])
ORDER BY id FOR UPDATE;

-- name: LockDelegationResultObligation :one
SELECT * FROM agent_task_delegation_return
WHERE task_id = @task_id AND delegation_return_id = @delegation_return_id
FOR UPDATE SKIP LOCKED;

-- name: GetDelegationResultObligationForTask :one
SELECT * FROM agent_task_delegation_return WHERE notification_task_id = @task_id;

-- name: MarkTaskResourceControlRequired :execrows
UPDATE agent_task_queue SET resource_control_required = true, session_pin_confirmed = false, session_pin_generation = @generation
WHERE id = @task_id AND runtime_id = @runtime_id AND dispatched_at = @dispatched_at
  AND status = 'dispatched';

-- name: RecordTaskTerminalReport :execrows
UPDATE agent_task_queue SET terminal_reported_at = COALESCE(terminal_reported_at, now())
WHERE id = @task_id AND status IN ('completed', 'failed', 'cancelled');

-- name: RecordReleasedTaskWorkDir :one
UPDATE agent_task_queue SET durable_work_dir = @durable_work_dir
WHERE id = @task_id AND status IN ('completed', 'failed', 'cancelled')
  AND work_dir = @work_dir
  AND (durable_work_dir IS NULL OR durable_work_dir = @durable_work_dir)
RETURNING *;

-- name: SetDelegationResultDelivery :exec
UPDATE agent_task_delegation_return
SET notification_delivery_id = @delivery_id, materialized_at = now()
WHERE task_id = @task_id AND delegation_return_id = @delegation_return_id
  AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL;

-- name: SetDelegationResultTask :exec
UPDATE agent_task_delegation_return
SET notification_task_id = @notification_task_id, notification_delivery_id = @notification_delivery_id, materialized_at = now()
WHERE task_id = @task_id AND delegation_return_id = @delegation_return_id
  AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL;

-- name: FailDelegationResultNotification :exec
UPDATE agent_task_delegation_return SET notification_error = @reason, materialized_at = now()
WHERE task_id = @task_id AND delegation_return_id = @delegation_return_id
  AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL;

-- name: CreateDelegationResultTask :one
INSERT INTO agent_task_queue (
    id, agent_id, runtime_id, issue_id, chat_session_id, autopilot_run_id,
    status, priority, context, session_id, work_dir, max_attempts,
    originator_user_id, accountable_user_id, originator_source, trigger_summary,
    delegated_from_task_id, trigger_evidence_kind, trigger_evidence_ref_id
)
SELECT @id, @agent_id, @runtime_id, sqlc.narg(issue_id), sqlc.narg(chat_session_id), sqlc.narg(autopilot_run_id),
    'queued', 0, @context, @session_id, @work_dir, 1,
    sqlc.narg(originator_user_id), sqlc.narg(accountable_user_id), @originator_source, @trigger_summary,
    sqlc.narg(delegated_from_task_id), @trigger_evidence_kind, @trigger_evidence_ref_id
WHERE lock_task_owner_rows(@agent_id, sqlc.narg(issue_id), @runtime_id)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListTaskResultNotifications :many
SELECT l.*, (SELECT current.id FROM issue_delegation_return current WHERE current.workspace_id = r.workspace_id AND current.issue_id = r.issue_id AND current.dispatcher_agent_id = r.dispatcher_agent_id AND current.is_current) AS current_return_id, r.dispatcher_agent_id, r.return_kind, r.source_task_id,
    r.external_session_id, r.runtime_id, r.provider, r.session_id, r.work_dir,
    delivery.delivery_status, delivery.processing_status, delivery.host_receipt,
    delivery.last_error AS delivery_error,
    handling.status AS handling_status, handling.started_at AS handling_started_at, handling.failure_reason AS handling_failure_reason,
    handling.error AS handling_error
FROM agent_task_delegation_return l
JOIN issue_delegation_return r ON r.id = l.delegation_return_id
LEFT JOIN agent_mailbox_message delivery ON delivery.id = l.notification_delivery_id
LEFT JOIN agent_task_queue handling ON handling.id = l.notification_task_id
WHERE l.task_id = @task_id AND r.workspace_id = @workspace_id
ORDER BY r.created_at, r.id;

-- name: PropagateResultSourceReturns :execrows
-- A result turn continues the accepted callers' work. The frozen address's
-- source task is only a conversation anchor, never caller provenance. A one-off takeover
-- carries no continuation, and a return already traversed by this outcome may
-- not be traversed again.
WITH RECURSIVE recipients AS (
 SELECT incoming.*, CASE WHEN handling.status = 'cancelled' AND mailbox.processing_status = 'handled'
                              AND mailbox.handled_by_task_id IS NOT NULL
                         THEN mailbox.handled_by_task_id ELSE incoming.notification_task_id END AS consumer_task_id
 FROM agent_task_delegation_return incoming
 JOIN agent_task_queue handling ON handling.id = incoming.notification_task_id
 LEFT JOIN agent_mailbox_message mailbox ON mailbox.id = incoming.notification_delivery_id
), proposed AS (
    SELECT incoming.consumer_task_id AS task_id, upstream.delegation_return_id,
           CASE WHEN bool_or(upstream.caller_task_ids IS NULL) THEN NULL
                ELSE COALESCE(array_agg(DISTINCT parent_caller) FILTER (WHERE parent_caller IS NOT NULL), '{}') END AS caller_task_ids
    FROM recipients incoming
    JOIN LATERAL unnest(incoming.caller_task_ids) caller ON true
    JOIN agent_task_delegation_return upstream ON upstream.task_id = caller
    LEFT JOIN LATERAL unnest(upstream.caller_task_ids) parent_caller ON true
    WHERE incoming.notification_task_id IS NOT NULL
      AND incoming.takeover_id IS NULL AND upstream.takeover_id IS NULL
      AND (sqlc.narg(task_id)::uuid IS NULL OR incoming.consumer_task_id = sqlc.narg(task_id))
    GROUP BY incoming.consumer_task_id, upstream.delegation_return_id
), candidates AS (
    SELECT proposed.* FROM proposed
    WHERE NOT EXISTS (
          SELECT 1 FROM agent_task_delegation_return present
          WHERE present.task_id = proposed.task_id
            AND present.delegation_return_id = proposed.delegation_return_id
            AND (present.caller_task_ids IS NULL OR (proposed.caller_task_ids IS NOT NULL AND proposed.caller_task_ids <@ present.caller_task_ids))
      )
), traversed AS (
    SELECT candidate.task_id AS continuation_task_id, prior.task_id, prior.delegation_return_id
    FROM candidates candidate
    JOIN recipients prior ON prior.consumer_task_id = candidate.task_id
    UNION
    SELECT next.continuation_task_id, prior.task_id, prior.delegation_return_id
    FROM recipients prior
    JOIN traversed next ON prior.consumer_task_id = next.task_id
), missing AS (
    SELECT candidate.* FROM candidates candidate
    WHERE NOT EXISTS (
        SELECT 1 FROM traversed
        WHERE continuation_task_id = candidate.task_id
          AND delegation_return_id = candidate.delegation_return_id
    )
    ORDER BY candidate.task_id, candidate.delegation_return_id
    LIMIT sqlc.narg(batch_size)
)
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, caller_task_ids)
SELECT task_id, delegation_return_id, caller_task_ids FROM missing
ON CONFLICT (task_id, delegation_return_id) DO UPDATE SET caller_task_ids =
CASE WHEN agent_task_delegation_return.caller_task_ids IS NULL OR EXCLUDED.caller_task_ids IS NULL THEN NULL
ELSE ARRAY(SELECT DISTINCT id FROM unnest(agent_task_delegation_return.caller_task_ids || EXCLUDED.caller_task_ids) id ORDER BY id) END;

-- name: ListDaemonResourceTasks :many
SELECT t.id AS task_id, t.agent_id, t.runtime_id, t.issue_id, t.chat_session_id,
       t.autopilot_run_id, t.status, t.session_id, t.work_dir,
       t.resource_control_required, t.terminal_reported_at,
       rt.workspace_id, rt.provider
FROM agent_task_queue t JOIN agent_runtime rt ON rt.id = t.runtime_id
WHERE rt.daemon_id = @daemon_id AND rt.workspace_id = @workspace_id
  AND (t.status NOT IN ('completed', 'failed', 'cancelled')
    OR (t.resource_control_required AND t.terminal_reported_at IS NULL)
    OR t.id = sqlc.narg(source_task_id)
    OR EXISTS (SELECT 1 FROM issue_delegation_return r WHERE r.source_task_id = t.id OR r.scope_task_id = t.id));

-- name: ListDaemonResourceReturns :many
SELECT r.* FROM issue_delegation_return r
LEFT JOIN agent_runtime rt ON rt.id = r.runtime_id
WHERE r.workspace_id = @workspace_id AND r.return_kind = 'managed_task'
  AND (rt.daemon_id = @daemon_id OR rt.id IS NULL);

-- name: ListReturnResultDependencies :many
SELECT sqlc.embed(l), t.status AS execution_status, t.delegation_outcome_final,
       t.resource_control_required, t.terminal_reported_at,
       handling.status AS handling_status, handling.started_at AS handling_started_at, handling.failure_reason AS handling_failure_reason,
       handling.terminal_reported_at AS handling_terminal_reported_at,
       handling.resource_control_required AS handling_resource_control_required,
       mailbox.processing_status AS mailbox_processing_status
FROM agent_task_delegation_return l
LEFT JOIN agent_task_queue t ON t.id = l.task_id
LEFT JOIN agent_task_queue handling ON handling.id = l.notification_task_id
LEFT JOIN agent_mailbox_message mailbox ON mailbox.id = l.notification_delivery_id
WHERE l.delegation_return_id = @return_id;

-- name: ConfirmTaskSessionPin :execrows
UPDATE agent_task_queue SET session_pin_confirmed = true
WHERE id = @task_id AND session_id = @session_id AND work_dir = @work_dir
  AND resource_control_required AND status = 'running' AND session_pin_generation = @generation;

-- name: HasPendingReturnInputs :one
SELECT EXISTS (
 SELECT 1 FROM comment_dispatch_obligation o
 LEFT JOIN comment_delegation_return c ON c.comment_id = o.comment_id
 WHERE o.status = 'pending' AND (o.delegation_return_id = @return_id OR c.delegation_return_id = @return_id)
) AS pending;

-- name: ListIssueResultNotifications :many
SELECT l.*, (SELECT current.id FROM issue_delegation_return current WHERE current.workspace_id = r.workspace_id AND current.issue_id = r.issue_id AND current.dispatcher_agent_id = r.dispatcher_agent_id AND current.is_current) AS current_return_id, r.dispatcher_agent_id, r.return_kind, r.source_task_id,
    r.external_session_id, r.runtime_id, r.provider, r.session_id, r.work_dir,
    delivery.delivery_status, delivery.processing_status, delivery.host_receipt,
    delivery.last_error AS delivery_error,
    handling.status AS handling_status, handling.started_at AS handling_started_at, handling.failure_reason AS handling_failure_reason,
    handling.error AS handling_error
FROM agent_task_delegation_return l
JOIN issue_delegation_return r ON r.id = l.delegation_return_id
JOIN agent_task_queue outcome ON outcome.id = l.task_id
LEFT JOIN agent_mailbox_message delivery ON delivery.id = l.notification_delivery_id
LEFT JOIN agent_task_queue handling ON handling.id = l.notification_task_id
WHERE r.issue_id = @issue_id AND r.workspace_id = @workspace_id
  AND (l.notification_task_id IS NOT NULL OR l.notification_delivery_id IS NOT NULL OR l.notification_error IS NOT NULL
    OR ((outcome.status = 'completed' OR (outcome.status = 'failed' AND outcome.delegation_outcome_final))
      AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = outcome.id)))
ORDER BY r.created_at, r.id;


-- name: ListManagedReturnsForScope :many
SELECT r.* FROM issue_delegation_return r
JOIN agent_task_queue source ON source.id = r.source_task_id
LEFT JOIN agent_task_queue root ON root.id = r.scope_task_id
WHERE r.workspace_id = @workspace_id AND r.return_kind = 'managed_task'
  AND ((sqlc.narg(issue_id)::uuid IS NOT NULL AND (source.issue_id = sqlc.narg(issue_id) OR root.issue_id = sqlc.narg(issue_id) OR r.issue_id = sqlc.narg(issue_id)))
    OR (sqlc.narg(chat_session_id)::uuid IS NOT NULL AND (source.chat_session_id = sqlc.narg(chat_session_id) OR root.chat_session_id = sqlc.narg(chat_session_id))))
 AND (sqlc.narg(after_return_id)::uuid IS NULL OR r.id > sqlc.narg(after_return_id))
ORDER BY r.id LIMIT 500;

-- name: CancelUnstartedDelegationResultTasks :many
UPDATE agent_task_queue t
SET status = 'cancelled', completed_at = now(), error = 'Issue closed before result notification started'
WHERE t.id IN (
 SELECT l.notification_task_id FROM agent_task_delegation_return l
 JOIN issue_delegation_return r ON r.id = l.delegation_return_id
 WHERE r.workspace_id = @workspace_id AND r.issue_id = @issue_id
)
AND t.status IN ('queued', 'deferred', 'dispatched', 'waiting_local_directory')
AND t.started_at IS NULL
RETURNING t.*;

-- name: InvalidateTaskSessionPin :one
UPDATE agent_task_queue
SET session_pin_generation = @next_generation,
    session_pin_confirmed = CASE WHEN session_pin_generation = @generation THEN false ELSE session_pin_confirmed END
WHERE id = @task_id AND resource_control_required AND status = 'running'
  AND session_pin_generation IN (@generation, @next_generation)
  AND COALESCE(context->>'type', '') <> 'delegation_result'
RETURNING *;

-- name: LockTaskSessionPin :one
SELECT * FROM agent_task_queue WHERE id = @task_id FOR UPDATE;

-- name: RecordDelegationResultAttempt :exec
UPDATE agent_task_delegation_return SET last_attempt_at = now()
WHERE task_id = @task_id AND delegation_return_id = @return_id
  AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL;

-- name: RecordDelegationRetryDecisionAttempt :exec
UPDATE agent_task_delegation_return SET last_attempt_at = now()
WHERE task_id = @task_id
  AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL;

-- name: ListManagedReturnsForRuntime :many
SELECT * FROM issue_delegation_return WHERE runtime_id = @runtime_id AND return_kind = 'managed_task'
 AND (sqlc.narg(after_return_id)::uuid IS NULL OR id > sqlc.narg(after_return_id))
ORDER BY id LIMIT 500;

-- name: ListManagedReturnsForWorkspace :many
SELECT * FROM issue_delegation_return WHERE workspace_id = @workspace_id AND return_kind = 'managed_task'
 AND (sqlc.narg(after_return_id)::uuid IS NULL OR id > sqlc.narg(after_return_id))
ORDER BY id LIMIT 500;

-- name: GetResourceTaskState :one
SELECT t.id,t.agent_id,t.status,t.resource_control_required,t.terminal_reported_at
FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id
WHERE t.id = @task_id AND a.workspace_id = @workspace_id;

-- name: LockTasksForScopeRemoval :exec
WITH locked AS (
 SELECT id FROM agent_task_queue
 WHERE (sqlc.narg(issue_id)::uuid IS NOT NULL AND issue_id = sqlc.narg(issue_id))
    OR (sqlc.narg(chat_session_id)::uuid IS NOT NULL AND chat_session_id = sqlc.narg(chat_session_id))
 ORDER BY id FOR UPDATE
)
SELECT count(*) FROM locked;

-- name: HasAgentResourceOnDaemon :one
SELECT EXISTS (
 SELECT 1 FROM agent_runtime rt
 WHERE rt.workspace_id = @workspace_id AND rt.daemon_id = @daemon_id AND rt.provider = @provider
   AND (rt.id = (SELECT a.runtime_id FROM agent a WHERE a.id = @agent_id AND a.workspace_id = @workspace_id)
     OR EXISTS (SELECT 1 FROM agent_task_queue t WHERE t.agent_id = @agent_id AND t.runtime_id = rt.id))
) AS hosted;
