ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_coordinator_check;
ALTER TABLE issue
    DROP COLUMN IF EXISTS coordinator_type,
    DROP COLUMN IF EXISTS coordinator_id,
    DROP COLUMN IF EXISTS merge_allowed,
    DROP COLUMN IF EXISTS merge_repository_url,
    DROP COLUMN IF EXISTS merge_target_branch,
    DROP COLUMN IF EXISTS merge_granted_by,
    DROP COLUMN IF EXISTS merge_granted_at;
