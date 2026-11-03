-- name: GetResultTakeoverByRequest :one
SELECT * FROM delegation_result_takeover
WHERE workspace_id = @workspace_id AND client_request_id = @client_request_id;

-- name: CreateResultTakeover :one
INSERT INTO delegation_result_takeover (
 id, workspace_id, client_request_id, result_task_id, from_return_id,
 expected_current_return_id, target_return_id, target_kind, target_selector_id, created_by
) VALUES (
 @id, @workspace_id, @client_request_id, @result_task_id, @from_return_id,
 @expected_current_return_id, @target_return_id, @target_kind, @target_selector_id, @created_by
) RETURNING *;

-- name: CreateDelegationReturnVersion :one
INSERT INTO issue_delegation_return (
 id, workspace_id, issue_id, dispatcher_agent_id, return_kind,
 external_session_id, source_task_id, runtime_id, provider, session_id, work_dir, scope_task_id, is_current
) VALUES (
 @id, @workspace_id, @issue_id, @dispatcher_agent_id, @return_kind,
 sqlc.narg(external_session_id), sqlc.narg(source_task_id), sqlc.narg(runtime_id),
 sqlc.narg(provider), sqlc.narg(session_id), sqlc.narg(work_dir), sqlc.narg(scope_task_id), false
) RETURNING *;

-- name: RetireCurrentDelegationReturn :exec
UPDATE issue_delegation_return SET is_current = false
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
 AND dispatcher_agent_id = @dispatcher_agent_id AND is_current;

-- name: SetCurrentDelegationReturn :exec
UPDATE issue_delegation_return SET is_current = true WHERE id = @id;

-- name: GetResultReturnObligation :one
SELECT * FROM agent_task_delegation_return
WHERE task_id = @task_id AND delegation_return_id = @delegation_return_id;

-- name: CreateTakeoverResultObligation :exec
INSERT INTO agent_task_delegation_return (task_id, delegation_return_id, takeover_id)
VALUES (@task_id, @delegation_return_id, @takeover_id);

-- name: LockResultTakeoverMember :one
SELECT * FROM member WHERE workspace_id = @workspace_id AND user_id = @user_id FOR SHARE;
