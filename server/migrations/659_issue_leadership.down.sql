CREATE OR REPLACE FUNCTION capture_comment_wakeup() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c comment; event_type text; source_id uuid; source_agent uuid; payload jsonb; root_id uuid;
BEGIN
 c := CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
 IF NOT EXISTS (SELECT 1 FROM issue_wakeup WHERE issue_id=c.issue_id AND enabled AND kind='event') THEN RETURN c; END IF;
 source_id := NULLIF(current_setting('multica.source_task_id',true),'')::uuid;
 IF TG_OP='INSERT' THEN
  event_type := 'comment.created';
  source_id := COALESCE(source_id,c.source_task_id);
  source_agent := CASE WHEN c.author_type='agent' THEN c.author_id END;
 ELSIF TG_OP='DELETE' THEN
  -- Pruning an existing tombstone is storage cleanup, not another deletion.
  IF OLD.deleted_at IS NOT NULL THEN RETURN OLD; END IF;
  event_type := 'comment.deleted';
 ELSIF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
  event_type := 'comment.deleted';
 ELSIF NEW.deleted_at IS NOT NULL THEN RETURN NEW;
 ELSIF OLD.resolved_at IS NULL AND NEW.resolved_at IS NOT NULL THEN event_type := 'comment.resolved';
 ELSIF OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS NULL THEN event_type := 'comment.unresolved';
 ELSIF NEW.content IS DISTINCT FROM OLD.content THEN event_type := 'comment.updated';
 ELSE RETURN NEW;
 END IF;
 IF source_id IS NOT NULL THEN SELECT agent_id INTO source_agent FROM agent_task_queue WHERE id=source_id; END IF;
 -- Keep author and actor distinct: an admin editing an agent's comment is
 -- not an event produced by that agent's original run.
 IF TG_OP<>'INSERT' AND source_id IS NULL THEN
  source_agent := CASE WHEN current_setting('multica.actor_type',true)='agent' THEN NULLIF(current_setting('multica.actor_id',true),'')::uuid END;
 END IF;
 root_id := c.id;
 IF c.parent_id IS NOT NULL THEN
  WITH RECURSIVE ancestors AS (
   SELECT id,parent_id,1 AS depth FROM comment WHERE id=c.parent_id AND issue_id=c.issue_id AND workspace_id=c.workspace_id
   UNION ALL SELECT p.id,p.parent_id,a.depth+1 FROM comment p JOIN ancestors a ON p.id=a.parent_id
    WHERE p.issue_id=c.issue_id AND p.workspace_id=c.workspace_id AND a.depth<256
  ) SELECT id INTO root_id FROM ancestors ORDER BY depth DESC LIMIT 1;
 END IF;
 payload := jsonb_build_object('comment_id',c.id,'parent_comment_id',c.parent_id,'thread_id',COALESCE(root_id,c.id),
  'author_type',c.author_type,'author_id',c.author_id);
 IF TG_OP='INSERT' THEN payload := payload || jsonb_build_object('actor_type',c.author_type,'actor_id',c.author_id); END IF;
 PERFORM capture_issue_wakeup(c.issue_id,event_type,gen_random_uuid()::text,source_agent,source_id,payload);
 RETURN c;
END $$;

ALTER TABLE issue_wakeup DROP COLUMN IF EXISTS target_session_id, DROP COLUMN IF EXISTS leadership_root_issue_id, DROP COLUMN IF EXISTS leadership_revision;
DROP TRIGGER IF EXISTS guard_task_session_write ON issue;
DROP FUNCTION IF EXISTS guard_task_session_write();
DROP TRIGGER IF EXISTS check_task_leadership ON agent_task_queue;
DROP TRIGGER IF EXISTS capture_task_leadership ON agent_task_queue;
DROP FUNCTION IF EXISTS capture_task_leadership();
DROP TRIGGER IF EXISTS capture_mailbox_leadership ON agent_mailbox_message;
DROP FUNCTION IF EXISTS capture_mailbox_leadership();
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS leadership_root_issue_id, DROP COLUMN IF EXISTS leadership_revision;
ALTER TABLE agent_mailbox_message DROP COLUMN IF EXISTS leadership_root_issue_id, DROP COLUMN IF EXISTS leadership_revision;
DROP TRIGGER IF EXISTS stamp_activity_actor_session ON activity_log;
DROP FUNCTION IF EXISTS stamp_actor_session();
DROP FUNCTION IF EXISTS issue_contract_basis(issue);
CREATE OR REPLACE FUNCTION issue_contract_basis(i issue) RETURNS JSONB
LANGUAGE sql STABLE AS $fn$
SELECT jsonb_build_object(
    'executor', jsonb_build_object('type', i.assignee_type, 'id', i.assignee_id),
    'acceptor', jsonb_build_object('type', i.coordinator_type, 'id', i.coordinator_id),
    'merge_authorization', jsonb_build_object('allowed', i.merge_allowed,
        'repository_url', i.merge_repository_url, 'target_branch', i.merge_target_branch,
        'granted_by', i.merge_granted_by, 'granted_at', i.merge_granted_at),
    'criteria', i.acceptance_criteria
) || CASE WHEN i.assignee_type = 'squad' THEN jsonb_build_object(
    'executor_representative', (
        SELECT jsonb_build_object('type', 'agent', 'id', a.id)
        FROM squad s JOIN agent a ON a.id = s.leader_id AND a.workspace_id = s.workspace_id
        WHERE s.id = i.assignee_id AND s.workspace_id = i.workspace_id
          AND s.archived_at IS NULL AND a.archived_at IS NULL AND a.kind = 'user'
          AND EXISTS (SELECT 1 FROM squad_member sm WHERE sm.squad_id = s.id
                      AND sm.member_type = 'agent' AND sm.member_id = a.id)
    )
) ELSE '{}'::jsonb END;
$fn$;

DROP FUNCTION IF EXISTS issue_contract_legacy_basis(issue);
DROP FUNCTION IF EXISTS issue_leader_id(issue, UUID);
ALTER TABLE activity_log DROP COLUMN IF EXISTS actor_session_id;
ALTER TABLE comment DROP COLUMN IF EXISTS actor_session_id;
ALTER TABLE issue DROP COLUMN IF EXISTS leadership_root_issue_id,
    DROP COLUMN IF EXISTS leadership_squad_id, DROP COLUMN IF EXISTS leader_agent_id,
    DROP COLUMN IF EXISTS leader_external_session_id, DROP COLUMN IF EXISTS leadership_revision,
    DROP COLUMN IF EXISTS creator_session_id, DROP COLUMN IF EXISTS session_semantics, DROP COLUMN IF EXISTS requester_leadership_root_id;

ALTER TABLE delegation_result_takeover ALTER COLUMN expected_current_return_id SET NOT NULL;

DROP FUNCTION IF EXISTS issue_actor_session_conflict(UUID,UUID,UUID,UUID);
