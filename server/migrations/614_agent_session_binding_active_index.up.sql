CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_agent_session_binding_active
    ON external_agent_session_binding (workspace_id, agent_id, issue_id)
    WHERE revoked_at IS NULL;
