CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_tgit_snapshot_refresh
    ON vcs_pull_request (snapshot_refresh_after ASC NULLS FIRST, id)
    WHERE provider = 'tgit' AND state IN ('open', 'draft');
