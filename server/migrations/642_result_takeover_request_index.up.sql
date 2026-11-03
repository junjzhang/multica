CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_delegation_result_takeover_request
    ON delegation_result_takeover (workspace_id, client_request_id);
