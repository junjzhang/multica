DROP TABLE IF EXISTS external_agent_token;
DROP TABLE IF EXISTS external_agent_session;
ALTER TABLE agent DROP COLUMN IF EXISTS execution_kind;
