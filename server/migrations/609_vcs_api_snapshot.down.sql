ALTER TABLE IF EXISTS vcs_pull_request
    DROP COLUMN IF EXISTS api_snapshot,
    DROP COLUMN IF EXISTS snapshot_fetched_at,
    DROP COLUMN IF EXISTS snapshot_refresh_after,
    DROP COLUMN IF EXISTS snapshot_error;
