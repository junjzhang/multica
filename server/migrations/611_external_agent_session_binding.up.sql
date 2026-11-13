-- A physical host session is its own record; which issue it currently serves
-- for an agent is a binding, not a property of the session. One agent may own
-- several sessions and one session may serve several issues, but an exact
-- (workspace, agent, issue) key has exactly one active target. Producers read
-- this binding before enqueueing and freeze the resulting session on the
-- delivery, so a later rebind never retargets an existing message.
CREATE TABLE external_agent_session_binding (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    session_id UUID NOT NULL,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
