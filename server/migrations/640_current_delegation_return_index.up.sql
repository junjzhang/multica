CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_current_issue_delegation_return
    ON issue_delegation_return (workspace_id, issue_id, dispatcher_agent_id) WHERE is_current;
