-- name: HasOtherLiveManagedScopeTask :one
-- Check scope occupancy inside the dispatch transaction. Result consumers
-- occupy this scope just like ordinary runs; owner locks also protect identity.
SELECT EXISTS (
 SELECT 1 FROM agent_task_queue
 WHERE agent_id = @agent_id AND issue_id = @issue_id AND id <> @task_id
   AND status NOT IN ('completed', 'failed', 'cancelled')
) AS present;

-- name: SetManagedReturnPredecessor :exec
UPDATE issue_delegation_return SET continued_from_return_id = @previous_id WHERE id = @id;

-- name: GetManagedReturnContinuation :one
-- Follow only certified continuation edges, never an explicit takeover or a
-- coincidentally newer session. The caller holds the delegated issue write
-- fence used by dispatch and takeover. UNION also bounds malformed cyclic data.
WITH RECURSIVE chain AS (
 SELECT r.* FROM issue_delegation_return r
 WHERE r.workspace_id = @workspace_id AND r.issue_id = @issue_id
   AND r.dispatcher_agent_id = @agent_id AND r.is_current
   AND r.return_kind = 'managed_task'
 UNION
 SELECT prior.* FROM issue_delegation_return prior
 JOIN chain next ON next.continued_from_return_id = prior.id
 JOIN agent_task_queue source ON source.id = prior.source_task_id
 WHERE prior.workspace_id = next.workspace_id AND prior.issue_id = next.issue_id
   AND prior.dispatcher_agent_id = next.dispatcher_agent_id
   AND prior.return_kind = 'managed_task' AND prior.runtime_id = next.runtime_id
   AND prior.provider = next.provider
   AND source.status IN ('completed', 'failed', 'cancelled')
)
SELECT r.* FROM issue_delegation_return r
WHERE r.workspace_id = @workspace_id AND r.issue_id = @issue_id
 AND r.dispatcher_agent_id = @agent_id AND r.is_current AND r.id <> @from_return_id
 AND EXISTS (SELECT 1 FROM chain WHERE chain.id = @from_return_id);
