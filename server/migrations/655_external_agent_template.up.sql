-- A host template is an agent row whose identity is the host instance it
-- registers sessions for. Both columns stay NULL on legacy per-session agents
-- and on managed agents, so an existing identity can never be reinterpreted as
-- a template by this migration.
ALTER TABLE agent
    ADD COLUMN external_provider TEXT,
    ADD COLUMN external_host_id TEXT;
