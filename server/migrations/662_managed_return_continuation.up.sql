ALTER TABLE issue_delegation_return ADD COLUMN continued_from_return_id UUID;
ALTER TABLE agent_task_delegation_return ADD COLUMN delivery_return_id UUID;
