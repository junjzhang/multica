-- Deploy the current-address writer with this migration; old writers cannot
-- infer ON CONFLICT from the retired all-versions index.
DROP INDEX CONCURRENTLY IF EXISTS uq_issue_delegation_return_dispatch;
