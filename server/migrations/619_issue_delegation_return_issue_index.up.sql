-- One accepted address per (issue, dispatcher). Concurrent dispatches for the
-- same actor race on this index; the winner's row is that actor's accepted
-- recipient and the loser re-reads it instead of replacing it. Another actor
-- has its own key and therefore its own address, and the same actor keeps one
-- address across assignment slots.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_delegation_return_dispatch
    ON issue_delegation_return (workspace_id, issue_id, dispatcher_agent_id);
