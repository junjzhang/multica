-- name: GetIssueContract :one
SELECT data FROM issue_contract WHERE issue_id = $1 AND workspace_id = $2;

-- name: SaveIssueContract :exec
INSERT INTO issue_contract (issue_id, workspace_id, data) VALUES ($1, $2, $3)
ON CONFLICT (issue_id) DO UPDATE SET data = EXCLUDED.data
WHERE issue_contract.workspace_id = EXCLUDED.workspace_id;

-- name: GetIssueContractBasis :one
SELECT issue_contract_basis(issue)::jsonb AS basis
FROM issue WHERE id = $1 AND workspace_id = $2;

-- name: SetIssueContractCriteria :exec
UPDATE issue SET acceptance_criteria = $3, status = 'in_progress' WHERE id = $1 AND workspace_id = $2;
