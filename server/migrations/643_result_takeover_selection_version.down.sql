-- Rollback is supported only with application and daemon writers stopped.
-- Never erase accepted actions or reinterpret historical addresses as current.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM delegation_result_takeover)
       OR EXISTS (SELECT 1 FROM agent_task_delegation_return WHERE takeover_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM issue_delegation_return WHERE is_current IS DISTINCT FROM true)
       OR EXISTS (
           SELECT 1 FROM issue_delegation_return
           GROUP BY workspace_id, issue_id, dispatcher_agent_id HAVING count(*) > 1
       ) THEN
        RAISE EXCEPTION USING
            MESSAGE = 'cannot roll back result takeover while accepted history or incompatible return relationships exist',
            HINT = 'Keep the takeover-capable schema and application; do not delete history to force rollback.';
    END IF;
END
$$;

ALTER TABLE delegation_result_takeover DROP COLUMN IF EXISTS target_selection_version;
