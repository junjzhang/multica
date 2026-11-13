CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_leadership_root ON issue (workspace_id, leadership_root_issue_id) WHERE leadership_root_issue_id IS NOT NULL;
