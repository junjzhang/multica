CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_delegation_return_dispatch
    ON issue_delegation_return (workspace_id, issue_id, dispatcher_agent_id);
