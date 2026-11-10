-- Repeated registration of one host session must resolve to the same identity.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_agent_session_identity
    ON external_agent_session (workspace_id, owner_user_id, provider, host_id, session_id);
