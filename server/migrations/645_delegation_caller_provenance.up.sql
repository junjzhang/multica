-- NULL preserves unknown provenance on pre-upgrade intents. A known empty
-- set is an external dispatcher; managed dispatches snapshot actual callers.
ALTER TABLE comment_delegation_return ADD COLUMN caller_task_ids UUID[];
ALTER TABLE issue_assignment_delegation_return ADD COLUMN caller_task_ids UUID[];
ALTER TABLE agent_task_delegation_return ADD COLUMN caller_task_ids UUID[];
