CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_agent_token_hash
    ON external_agent_token (token_hash);
