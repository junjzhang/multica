ALTER TABLE agent_task_delegation_return DROP COLUMN IF EXISTS delivery_return_id;
ALTER TABLE issue_delegation_return DROP COLUMN IF EXISTS continued_from_return_id;
