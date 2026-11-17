-- name: CreateExternalAgentTemplate :one
-- A template is the stable host identity: sessions of this host attach to it
-- instead of minting one agent each. The key columns are part of the unique
-- template index; callers that lose a concurrent insert re-read by key.
INSERT INTO agent (
    workspace_id, name, description, runtime_mode, runtime_config, visibility,
    permission_mode, max_concurrent_tasks, owner_id, instructions, custom_env,
    custom_args, mcp_config, execution_kind, kind,
    external_provider, external_host_id
) VALUES (
    @workspace_id, @name, '', 'local', '{}'::jsonb, @visibility,
    @permission_mode, 1, @owner_id, '', '{}'::jsonb,
    '[]'::jsonb, NULL, 'external', 'user',
    @external_provider, @external_host_id
)
RETURNING *;

-- name: GetExternalAgentTemplateByKey :one
SELECT * FROM agent
WHERE workspace_id = @workspace_id
  AND owner_id = @owner_id
  AND external_provider = @external_provider
  AND external_host_id = @external_host_id;

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
-- Presentation only: an agent may own several sessions, and the frozen agent
-- DTO still carries one. The oldest session is the stable choice; no delivery
-- path may address a message through this lookup.
SELECT * FROM external_agent_session
WHERE agent_id = $1
ORDER BY created_at ASC, id ASC
LIMIT 1;

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

-- name: ListExternalAgentSessionsForRead :many
-- All registered host sessions of one external agent, with the two conditions
-- that make a session undeliverable at read time: the agent was archived, or
-- the session owner is no longer a workspace member. A missing binding is not
-- an error; the UI shows the agent's sessions without a current target.
SELECT s.*,
       (a.archived_at IS NOT NULL)::boolean AS agent_archived,
(s.revoked_at IS NOT NULL)::boolean AS session_revoked,
       EXISTS (
           SELECT 1 FROM member m
           WHERE m.workspace_id = s.workspace_id AND m.user_id = s.owner_user_id
       )::boolean AS owner_is_member
FROM external_agent_session s
JOIN agent a ON a.id = s.agent_id AND a.workspace_id = s.workspace_id
WHERE s.workspace_id = @workspace_id AND s.agent_id = @agent_id
ORDER BY s.created_at ASC, s.id ASC;

-- name: UpdateExternalAgentSessionProfile :one
-- Only presentation and the delivering daemon are mutable; the identity tuple
-- is immutable once registered. The exact session id scopes the write: an
-- agent may own several host sessions and re-registering one must never touch
-- a sibling's profile or daemon.
UPDATE external_agent_session
SET display_name = @display_name,
    url = sqlc.narg('url'),
    daemon_id = @daemon_id,
    updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id
RETURNING *;

-- name: RevokeExternalAgentSession :one
-- Revocation is idempotent and scoped to one session: siblings and the agent
-- identity keep working. Auth re-reads this column on every request.
UPDATE external_agent_session
SET revoked_at = COALESCE(revoked_at, now()),
    updated_at = now()
WHERE id = @id
  AND agent_id = @agent_id
  AND workspace_id = @workspace_id

RETURNING *;

-- name: WorkspaceDaemonOwnedByUser :one
SELECT EXISTS (
    SELECT 1 FROM agent_runtime
    WHERE workspace_id = @workspace_id AND daemon_id = @daemon_id AND owner_id = @owner_user_id
) AS exists;

-- name: CreateExternalAgentToken :one
-- external_session_id is the exact registered session the credential acts for.
-- Legacy tokens keep NULL; the auth path re-checks the session on every use.
INSERT INTO external_agent_token (
    id, agent_id, workspace_id, owner_user_id, token_hash, expires_at,
    external_session_id
) VALUES (
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()), @agent_id, @workspace_id,
    @owner_user_id, @token_hash, @expires_at,
    sqlc.narg('external_session_id')
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

-- name: ListExternalAgentSessionsByHostSession :many
-- An external process names its host session (for Paseo, PASEO_AGENT_ID). The
-- caller must still scope the match to the authenticated agent and workspace;
-- more than one row means the reference is ambiguous and must be rejected
-- rather than resolved to an arbitrary host.
SELECT * FROM external_agent_session
WHERE workspace_id = @workspace_id
  AND agent_id = @agent_id
  AND session_id = @host_session_id
ORDER BY created_at ASC, id ASC;
