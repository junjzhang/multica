-- An external agent identity is exactly one host session.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_agent_session_agent
    ON external_agent_session (agent_id);
