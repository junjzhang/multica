-- External agents keep their execution inside a host session (Paseo) instead
-- of a Multica runtime. execution_kind is the explicit discriminator: an
-- unbound runtime_id already means "managed agent waiting for a runtime" and
-- must never be reinterpreted as external. Unknown values are not executable.
ALTER TABLE agent
    ADD COLUMN execution_kind TEXT NOT NULL DEFAULT 'managed'
    CHECK (execution_kind IN ('managed', 'external'));

-- One identity per concrete host session. display_name and url are mutable
-- presentation; (workspace, owner, provider, host_id, session_id) is what
-- identifies the session. agent_id is unique because an external agent IS one
-- session — register the same session twice and the same agent comes back.
CREATE TABLE external_agent_session (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    owner_user_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    provider TEXT NOT NULL DEFAULT 'paseo' CHECK (provider IN ('paseo')),
    host_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    url TEXT,
    daemon_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Independently revocable machine credential for one external agent. The raw
-- token is shown once at creation; only its hash is stored. Revocation is a
-- timestamp so an audit trail survives, and the auth path re-checks the
-- owner's workspace membership and the agent's archive state on every use.
CREATE TABLE external_agent_token (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    owner_user_id UUID NOT NULL,
    token_hash TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
