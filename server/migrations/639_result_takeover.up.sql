ALTER TABLE issue_delegation_return ADD COLUMN is_current BOOLEAN NOT NULL DEFAULT true;

CREATE TABLE delegation_result_takeover (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    client_request_id UUID NOT NULL,
    result_task_id UUID NOT NULL,
    from_return_id UUID NOT NULL,
    expected_current_return_id UUID NOT NULL,
    target_return_id UUID NOT NULL,
    target_kind TEXT NOT NULL CHECK (target_kind IN ('external_session', 'managed_task')),
    target_selector_id UUID NOT NULL,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE agent_task_delegation_return ADD COLUMN takeover_id UUID;
