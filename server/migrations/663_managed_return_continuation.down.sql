-- Stop application and daemon writers before rollback. A forwarded result's
-- physical destination cannot be reconstructed after dropping these proofs.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM issue_delegation_return WHERE continued_from_return_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM agent_task_delegation_return
                  WHERE delivery_return_id IS NOT NULL AND delivery_return_id <> delegation_return_id) THEN
        RAISE EXCEPTION USING
            MESSAGE = 'cannot roll back managed return continuation while accepted continuation or forwarding history exists',
            HINT = 'Retain the continuation-capable schema and application; do not delete history to force rollback.';
    END IF;
END
$$;

ALTER TABLE agent_task_delegation_return DROP COLUMN IF EXISTS delivery_return_id;
ALTER TABLE issue_delegation_return DROP COLUMN IF EXISTS continued_from_return_id;
