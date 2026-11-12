CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_external_agent_token_agent
    ON external_agent_token (agent_id, workspace_id);
