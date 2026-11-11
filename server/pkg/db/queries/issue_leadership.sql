-- name: GetIssueLeadershipRoot :one
SELECT r.* FROM issue i JOIN issue r
ON r.id=COALESCE(i.leadership_root_issue_id,i.id) AND r.workspace_id=i.workspace_id
WHERE i.id=$1 AND i.workspace_id=$2;

-- name: ClaimIssueLeadership :one
UPDATE issue SET leadership_root_issue_id=id, leadership_squad_id= @squad_id,
    leader_agent_id= @agent_id, leader_external_session_id=sqlc.narg('session_id')::uuid,
    leadership_revision=leadership_revision+1
WHERE id= @id AND workspace_id= @workspace_id AND (leadership_root_issue_id IS NULL OR leadership_squad_id IS DISTINCT FROM @squad_id)
    AND leadership_revision= @expected_revision
RETURNING *;

-- name: HandoffIssueLeadership :one
UPDATE issue SET leader_agent_id= @agent_id,
    leader_external_session_id=sqlc.narg('session_id')::uuid,
    leadership_revision=leadership_revision+1, revision=revision+1, updated_at=now()
WHERE id= @id AND workspace_id= @workspace_id AND leadership_root_issue_id=id
    AND leadership_revision= @expected_revision AND revision= @expected_issue_revision
RETURNING *;

-- name: SetIssueCreatorSession :exec
UPDATE issue SET creator_session_id= @session_id
WHERE id= @id AND workspace_id= @workspace_id AND creator_session_id IS NULL;

-- name: IssueActorSessionConflict :one
SELECT issue_actor_session_conflict(@workspace_id::uuid,@issue_id::uuid,@agent_id::uuid,@session_id::uuid)::boolean AS conflict;

-- name: ListLeadershipIssues :many
SELECT * FROM issue WHERE workspace_id= @workspace_id
 AND (id= @root_id OR leadership_root_issue_id= @root_id) ORDER BY id FOR UPDATE NOWAIT;

-- name: SquadHasLeadershipIssues :one
SELECT EXISTS (SELECT 1 FROM issue i JOIN issue r ON r.id=COALESCE(i.leadership_root_issue_id,i.id) AND r.workspace_id=i.workspace_id WHERE i.workspace_id=$1 AND r.leadership_squad_id=$2 AND issue_effective_status(i.workspace_id,i.status) NOT IN ('done','cancelled')) AS has_issues;

-- name: AdoptIssueSessionSemantics :exec
UPDATE issue SET session_semantics=true WHERE id=$1 AND workspace_id=$2;

-- name: WithdrawLeadershipDeliveries :exec
UPDATE agent_mailbox_message
SET delivery_status=CASE WHEN delivery_status='pending' THEN 'failed' ELSE delivery_status END,
 withdrawn_at=COALESCE(withdrawn_at,now()), updated_at=now(),
 last_error=CASE WHEN delivery_status='pending' THEN 'Task leadership handed off' ELSE last_error END
WHERE workspace_id= @workspace_id AND issue_id= @issue_id AND recipient_agent_id= @agent_id
 AND target_session_id IS NOT NULL AND kind IN ('issue_assignment','comment_mention','issue_wakeup')
 AND delivery_status IN ('pending','leased','unknown') AND withdrawn_at IS NULL;

-- name: CancelLeadershipQueuedTasks :exec
UPDATE agent_task_queue SET status='cancelled',completed_at=now()
WHERE issue_id= @issue_id AND agent_id= @agent_id AND is_leader_task=true
 AND status IN ('queued','deferred','waiting_local_directory') AND started_at IS NULL;

-- name: LockBoundLeadershipRoot :one
SELECT * FROM issue WHERE id=$1 AND workspace_id=$2 AND leadership_root_issue_id=id
FOR SHARE NOWAIT;

-- name: SetIssueRequesterLeadership :exec
UPDATE issue SET requester_leadership_root_id=$3 WHERE id=$1 AND workspace_id=$2;

-- name: AttachIssueLeadershipRoot :one
UPDATE issue SET leadership_root_issue_id= @root_id
WHERE id= @id AND workspace_id= @workspace_id AND leadership_root_issue_id IS NULL
RETURNING *;

-- name: PinIssueWakeupLeadership :one
UPDATE issue_wakeup SET target_session_id=sqlc.narg('target_session_id')::uuid,
 leadership_root_issue_id=sqlc.narg('root_id')::uuid,
 leadership_revision=sqlc.narg('leadership_revision')::bigint
WHERE id= @id AND workspace_id= @workspace_id RETURNING *;

-- name: ListLeadershipAffectedContracts :many
SELECT c.issue_id FROM issue_contract c JOIN issue i ON i.id=c.issue_id AND i.workspace_id=c.workspace_id
WHERE i.workspace_id= @workspace_id AND (i.id= @root_id OR i.leadership_root_issue_id= @root_id OR i.requester_leadership_root_id= @root_id)
ORDER BY c.issue_id;

-- name: ListLeadershipReferencingIssues :many
SELECT id FROM issue WHERE workspace_id= @workspace_id AND leadership_root_issue_id= @root_id AND id<> @root_id
ORDER BY id;

-- name: HandoffIssueExecutor :one
UPDATE issue SET assignee_id= @agent_id, revision=revision+1, updated_at=now(), session_semantics=true
WHERE id= @id AND workspace_id= @workspace_id AND assignee_type='agent'
 AND assignee_id= @previous_agent_id AND revision= @expected_issue_revision
RETURNING *;

-- name: CancelExecutorQueuedTasks :exec
UPDATE agent_task_queue SET status='cancelled',completed_at=now(),prepare_lease_expires_at=NULL
WHERE issue_id= @issue_id AND agent_id= @agent_id
 AND status IN ('queued','deferred','waiting_local_directory') AND started_at IS NULL;
