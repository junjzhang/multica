-- One obligation per (comment, executing agent). The comment write upserts the
-- row in its own transaction, so a retry cannot fan the same accepted target
-- out into two recovery obligations.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_comment_dispatch_obligation_target
    ON comment_dispatch_obligation (comment_id, agent_id);
