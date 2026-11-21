CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_record_request ON issue_record (issue_id, client_request_id);
