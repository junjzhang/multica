-- Every return retains its own inputs. Only a current managed version may
-- additionally inherit pending dependencies through certified continuation edges.
CREATE FUNCTION managed_return_continuation_chain(p_return_id UUID)
RETURNS TABLE(return_id UUID)
LANGUAGE sql STABLE AS $$
WITH RECURSIVE chain AS (
    SELECT p_return_id AS id, COALESCE((
        SELECT r.is_current AND r.return_kind = 'managed_task'
        FROM issue_delegation_return r WHERE r.id = p_return_id
    ), false) AS can_follow
    UNION
    SELECT prior.id, true
    FROM chain link
    JOIN issue_delegation_return next ON next.id = link.id
    JOIN issue_delegation_return prior ON prior.id = next.continued_from_return_id
    JOIN agent_task_queue source ON source.id = prior.source_task_id
    WHERE link.can_follow AND prior.return_kind = 'managed_task'
      AND prior.workspace_id = next.workspace_id AND prior.issue_id = next.issue_id
      AND prior.dispatcher_agent_id = next.dispatcher_agent_id
      AND prior.runtime_id = next.runtime_id AND prior.provider = next.provider
      AND source.status IN ('completed', 'failed', 'cancelled')
)
SELECT chain.id FROM chain;
$$;
