ALTER TABLE comment
ADD COLUMN external_originator_user_id uuid,
ADD CONSTRAINT comment_external_originator_source CHECK (
    external_originator_user_id IS NULL OR (author_type = 'agent' AND source_task_id IS NULL)
);
