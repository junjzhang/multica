-- Durable mailbox for requests addressed to an external agent session. The
-- message body and its reply relationship stay in `comment`; this row adds the
-- recipient, delivery and processing receipts. There is no foreign key by
-- repository rule: issue/comment/agent deletion cleans these rows up in
-- application code.
--
-- Unique (recipient_agent_id, comment_id) is the idempotency key: re-running
-- the same delivery attempt can never create a second request.
CREATE TABLE agent_mailbox_message (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    comment_id UUID NOT NULL,
    recipient_agent_id UUID NOT NULL,
    -- The sender's host session when the request came from an external agent;
    -- NULL for member-authored requests. This is the source of the message,
    -- not the recipient session (the claim response carries that separately).
    source_session_id UUID,
    sender_type TEXT NOT NULL CHECK (sender_type IN ('member', 'agent')),
    sender_id UUID NOT NULL,
    delivery_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'leased', 'delivered', 'failed')),
    processing_status TEXT NOT NULL DEFAULT 'unread'
        CHECK (processing_status IN ('unread', 'read', 'handled')),
    -- Delivery lease. The token is minted per claim, stored hashed, and ack
    -- must present both the token and the claiming daemon. Clearing it on ack
    -- makes a stale ack fail instead of overwriting a later lease.
    lease_token_hash TEXT,
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    host_receipt TEXT,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
