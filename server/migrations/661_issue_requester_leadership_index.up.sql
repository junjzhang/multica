CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_requester_leadership ON issue (workspace_id, requester_leadership_root_id) WHERE requester_leadership_root_id IS NOT NULL;
