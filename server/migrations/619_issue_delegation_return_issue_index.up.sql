-- One accepted delegation return per dispatcher and assignment slot on an
-- issue. Concurrent dispatches for the same key race on this index; the
-- winner's row is that dispatch's accepted recipient and the loser re-reads it
-- instead of replacing it. A different dispatcher or a different assignee has
-- its own key and therefore its own row.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_delegation_return_dispatch
    ON issue_delegation_return (workspace_id, issue_id, dispatcher_agent_id, assignee_type, assignee_id);
