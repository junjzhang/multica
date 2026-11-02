-- One accepted delegation return per issue. Concurrent dispatches for the same
-- issue race on this index; the winner's row is the accepted recipient and the
-- loser re-reads it instead of replacing it.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_delegation_return_issue
    ON issue_delegation_return (workspace_id, issue_id);
