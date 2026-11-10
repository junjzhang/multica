-- A new credential names the exact registered session it acts for. Legacy
-- tokens stay NULL; the auth path treats them as unbound and refuses
-- session-sensitive operations until they are reissued.
ALTER TABLE external_agent_token
    ADD COLUMN external_session_id UUID;
