-- One template per (workspace, owner, provider, host instance). The partial
-- predicate leaves legacy agents and managed agents out of the key entirely.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_agent_external_template_key
    ON agent (workspace_id, owner_id, external_provider, external_host_id)
    WHERE external_provider IS NOT NULL AND external_host_id IS NOT NULL;
