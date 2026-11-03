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
    SELECT 1 FROM agent_task_delegation_return l JOIN execution e ON e.id = l.task_id
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
  AND EXISTS (SELECT 1 FROM agent_task_delegation_return l WHERE l.task_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = t.id)
ORDER BY t.completed_at, t.id LIMIT @batch_size;

-- name: ListReadyDelegationResults :many
SELECT l.task_id, l.delegation_return_id
FROM agent_task_delegation_return l
JOIN agent_task_queue t ON t.id = l.task_id
WHERE l.notification_task_id IS NULL AND l.notification_delivery_id IS NULL
  AND l.notification_error IS NULL
  AND (t.status = 'completed' OR (t.status = 'failed' AND t.delegation_outcome_final))
  AND NOT EXISTS (SELECT 1 FROM agent_task_queue child WHERE child.retry_of_task_id = t.id)
ORDER BY t.completed_at, l.task_id, l.delegation_return_id
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

-- name: SetDelegationResultDelivery :exec
UPDATE agent_task_delegation_return
SET notification_delivery_id = @delivery_id, materialized_at = now()
WHERE task_id = @task_id AND delegation_return_id = @delegation_return_id
  AND notification_task_id IS NULL AND notification_delivery_id IS NULL AND notification_error IS NULL;

-- name: SetDelegationResultTask :exec
UPDATE agent_task_delegation_return
SET notification_task_id = @notification_task_id, materialized_at = now()
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
    originator_user_id, accountable_user_id, originator_source, trigger_summary
)
SELECT @id, @agent_id, @runtime_id, sqlc.narg(issue_id), sqlc.narg(chat_session_id), sqlc.narg(autopilot_run_id),
    'queued', 0, @context, @session_id, @work_dir, 1,
    sqlc.narg(originator_user_id), sqlc.narg(accountable_user_id), 'delegation', @trigger_summary
WHERE lock_task_owner_rows(@agent_id, sqlc.narg(issue_id), @runtime_id)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListTaskResultNotifications :many
SELECT l.*, r.dispatcher_agent_id, r.return_kind, r.source_task_id,
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

-- name: ConfirmTaskSessionPin :execrows
UPDATE agent_task_queue SET session_pin_confirmed = true
WHERE id = @task_id AND session_id = @session_id AND work_dir = @work_dir
  AND resource_control_required AND status = 'running' AND session_pin_generation = @generation;

-- name: ListIssueResultNotifications :many
SELECT l.*, r.dispatcher_agent_id, r.return_kind, r.source_task_id,
    r.external_session_id, r.runtime_id, r.provider, r.session_id, r.work_dir,
    delivery.delivery_status, delivery.processing_status, delivery.host_receipt,
    delivery.last_error AS delivery_error,
    handling.status AS handling_status, handling.started_at AS handling_started_at, handling.failure_reason AS handling_failure_reason,
    handling.error AS handling_error
FROM agent_task_delegation_return l
JOIN issue_delegation_return r ON r.id = l.delegation_return_id
LEFT JOIN agent_mailbox_message delivery ON delivery.id = l.notification_delivery_id
LEFT JOIN agent_task_queue handling ON handling.id = l.notification_task_id
WHERE r.issue_id = @issue_id AND r.workspace_id = @workspace_id
ORDER BY r.created_at, r.id;


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
