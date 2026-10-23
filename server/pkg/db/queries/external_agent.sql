-- name: CreateExternalAgent :one
-- An external agent has no Multica runtime: execution_kind marks the host
-- session as the executor, and runtime_id stays NULL so no managed claim can
-- pick it up. runtime_mode keeps its existing 'local' value because it still
-- describes where the agent's work happens from the caller's point of view;
-- clients branch on execution_kind, never on runtime_bound alone.
INSERT INTO agent (
    workspace_id, name, description, runtime_mode, runtime_config, visibility,
    permission_mode, max_concurrent_tasks, owner_id, instructions, custom_env,
    custom_args, mcp_config, execution_kind, kind
) VALUES (
    @workspace_id, @name, '', 'local', '{}'::jsonb, @visibility,
    @permission_mode, 1, @owner_id, '', '{}'::jsonb,
    '[]'::jsonb, NULL, 'external', 'user'
)
RETURNING *;

-- name: CreateExternalAgentSession :one
-- Returns no row when this host session is already registered (the unique
-- identity index wins). Callers re-read through the identity lookup, so a
-- repeat registration is idempotent and never mints a second identity.
INSERT INTO external_agent_session (
    id, workspace_id, owner_user_id, agent_id, provider, host_id, session_id,
    display_name, url, daemon_id
) VALUES (
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()), @workspace_id, @owner_user_id,
    @agent_id, @provider, @host_id, @session_id, @display_name, sqlc.narg('url'),
    @daemon_id
)
ON CONFLICT (workspace_id, owner_user_id, provider, host_id, session_id) DO NOTHING
RETURNING *;

-- name: GetExternalAgentSessionByAgentID :one
SELECT * FROM external_agent_session WHERE agent_id = $1;

-- name: GetExternalAgentSessionByID :one
SELECT * FROM external_agent_session WHERE id = $1 AND workspace_id = $2;

-- name: GetExternalAgentSessionByIdentity :one
SELECT * FROM external_agent_session
WHERE workspace_id = @workspace_id
  AND owner_user_id = @owner_user_id
  AND provider = @provider
  AND host_id = @host_id
  AND session_id = @session_id;

-- name: ListExternalAgentSessionsByAgentIDs :many
SELECT * FROM external_agent_session WHERE agent_id = ANY(@agent_ids::uuid[]);

-- name: ListExternalAgentSessionsByIDs :many
SELECT * FROM external_agent_session WHERE id = ANY(@ids::uuid[]);

-- name: UpdateExternalAgentSessionProfile :one
-- Only presentation and the delivering daemon are mutable; the identity tuple
-- is immutable once registered.
UPDATE external_agent_session
SET display_name = @display_name,
    url = sqlc.narg('url'),
    daemon_id = @daemon_id,
    updated_at = now()
WHERE agent_id = @agent_id
RETURNING *;

-- name: WorkspaceDaemonExists :one
-- An external session may only name a daemon this workspace has actually
-- registered. The claim path additionally pins the session to the daemon/owner
-- it was registered for.
SELECT EXISTS (
    SELECT 1 FROM agent_runtime WHERE workspace_id = $1 AND daemon_id = $2
) AS exists;

-- name: WorkspaceDaemonOwnedByUser :one
SELECT EXISTS (
    SELECT 1 FROM agent_runtime
    WHERE workspace_id = @workspace_id AND daemon_id = @daemon_id AND owner_id = @owner_user_id
) AS exists;

-- name: CreateExternalAgentToken :one
INSERT INTO external_agent_token (
    id, agent_id, workspace_id, owner_user_id, token_hash, expires_at
) VALUES (
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()), @agent_id, @workspace_id,
    @owner_user_id, @token_hash, @expires_at
)
RETURNING *;

-- name: GetExternalAgentTokenByHash :one
SELECT * FROM external_agent_token
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND expires_at > now();

-- name: RevokeExternalAgentToken :one
UPDATE external_agent_token
SET revoked_at = now()
WHERE id = @id
  AND agent_id = @agent_id
  AND workspace_id = @workspace_id
  AND revoked_at IS NULL
RETURNING *;
