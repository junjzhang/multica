-- Stop application and daemon readers before rolling back this shared query.
DROP FUNCTION IF EXISTS managed_return_continuation_chain(UUID);
