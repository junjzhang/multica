-- One accepted delegation return per dispatcher on an issue. Concurrent
-- dispatches by the same actor race on this index; the winner's row is that
-- actor's accepted recipient and the loser re-reads it instead of replacing it.
-- A different dispatcher has its own key and therefore its own row.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_delegation_return_dispatcher
    ON issue_delegation_return (workspace_id, issue_id, dispatcher_agent_id);
