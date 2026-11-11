-- name: GetIssueContract :one
SELECT data FROM issue_contract WHERE issue_id = $1 AND workspace_id = $2;

-- name: SaveIssueContract :exec
INSERT INTO issue_contract (issue_id, workspace_id, data) VALUES ($1, $2, $3)
ON CONFLICT (issue_id) DO UPDATE SET data = EXCLUDED.data
WHERE issue_contract.workspace_id = EXCLUDED.workspace_id;

-- name: GetIssueContractBasis :one
SELECT issue_contract_basis(issue)::jsonb AS basis
FROM issue WHERE id = $1 AND workspace_id = $2;

-- name: IssueHasContract :one
SELECT EXISTS (SELECT 1 FROM issue_contract WHERE issue_id=$1 AND workspace_id=$2)::boolean;

-- name: LockSquadForContract :one
-- Contract writes already hold their source task and issue. Do not wait on
-- a representative lock in the opposite order from assignment or teardown.
SELECT * FROM squad WHERE id=$1 AND workspace_id=$2 FOR SHARE NOWAIT;

-- name: LockAgentForContract :one
SELECT * FROM agent WHERE id=$1 AND workspace_id=$2 AND kind='user' FOR SHARE NOWAIT;

-- name: CaptureIssueContractWakeup :exec
SELECT capture_issue_wakeup(@issue_id::uuid, 'issue.updated', @event_key::text,
    NULL, sqlc.narg('source_task_id')::uuid,
    jsonb_build_object('changed_fields', jsonb_build_array('contract')));
