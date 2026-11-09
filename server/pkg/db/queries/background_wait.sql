-- name: SetManagedWaitContext :one
UPDATE agent_task_queue
SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('background_wait', @wait::jsonb)
WHERE id = @task_id
RETURNING *;

-- name: SuspendTaskForBackgroundWait :one
UPDATE agent_task_queue
SET status = 'deferred', started_at = NULL, dispatched_at = NULL,
    prepare_lease_expires_at = NULL, fire_at = sqlc.narg(wake_at),
    wait_reason = 'Waiting for background command',
    branch_name = COALESCE(NULLIF(@branch_name::text, ''), branch_name)
WHERE id = @task_id AND status IN ('running','dispatched','waiting_local_directory','deferred')
RETURNING *;

-- name: WakeTaskForBackgroundWait :exec
UPDATE agent_task_queue SET fire_at = now()
WHERE id = @task_id AND status = 'deferred';

-- name: ListRuntimeBackgroundWaitTasks :many
SELECT * FROM agent_task_queue
WHERE runtime_id = @runtime_id
  AND context->'background_wait'->'address'->>'task_id' = id::text
  AND context->'background_wait'->>'consumed' IS DISTINCT FROM 'true'
  AND status IN ('running','dispatched','waiting_local_directory','deferred')
ORDER BY id;

-- name: CreateManagedWaitMailbox :one
INSERT INTO agent_mailbox_message (
    id, workspace_id, issue_id, recipient_agent_id, source_type, source_key,
    kind, notification_key, payload
) VALUES (
    @id, @workspace_id, sqlc.narg(issue_id), @agent_id, 'background_wait', @notification_key,
    'background_wait', @notification_key, @payload
)
ON CONFLICT (workspace_id, notification_key) WHERE notification_key IS NOT NULL
DO UPDATE SET notification_key=EXCLUDED.notification_key
WHERE agent_mailbox_message.recipient_agent_id = EXCLUDED.recipient_agent_id
  AND agent_mailbox_message.source_type = 'background_wait'
  AND agent_mailbox_message.payload = EXCLUDED.payload
  AND agent_mailbox_message.target_session_id IS NULL
RETURNING *;
