-- Bounds the recovery scan to obligations that still need a run. Settled and
-- permanently failed rows are historical and never scanned.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_comment_dispatch_obligation_pending
    ON comment_dispatch_obligation (next_attempt_at)
    WHERE status = 'pending';
