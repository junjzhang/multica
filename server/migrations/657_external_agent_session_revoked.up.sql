-- Revocation is a timestamp so an audit trail survives. A revoked session keeps
-- its identity tuple and history; the auth path refuses new work for it.
ALTER TABLE external_agent_session
    ADD COLUMN revoked_at TIMESTAMPTZ;
