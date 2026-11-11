ALTER TABLE issue
    ADD COLUMN leadership_root_issue_id UUID,
    ADD COLUMN leadership_squad_id UUID,
    ADD COLUMN leader_agent_id UUID,
    ADD COLUMN leader_external_session_id UUID,
    ADD COLUMN leadership_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN creator_session_id UUID,
ADD COLUMN session_semantics BOOLEAN NOT NULL DEFAULT false,
ADD COLUMN requester_leadership_root_id UUID;

ALTER TABLE comment ADD COLUMN actor_session_id UUID;
ALTER TABLE activity_log ADD COLUMN actor_session_id UUID;

-- Existing contracts keep their legacy basis until a task adopts leadership.
CREATE FUNCTION issue_leader_id(i issue, squad_id UUID) RETURNS UUID
LANGUAGE sql STABLE AS $fn$
SELECT CASE WHEN r.leadership_squad_id = squad_id THEN r.leader_agent_id
    ELSE (SELECT s.leader_id FROM squad s JOIN agent a ON a.id=s.leader_id
          AND a.workspace_id=s.workspace_id AND a.execution_kind='managed'
          AND a.archived_at IS NULL WHERE s.id=squad_id
          AND s.workspace_id=i.workspace_id AND s.archived_at IS NULL) END
FROM (SELECT 1) seed LEFT JOIN issue r
ON r.id=COALESCE(i.leadership_root_issue_id,i.id) AND r.workspace_id=i.workspace_id;
$fn$;

CREATE OR REPLACE FUNCTION issue_contract_legacy_basis(i issue) RETURNS JSONB
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

CREATE OR REPLACE FUNCTION issue_contract_basis(i issue) RETURNS JSONB
LANGUAGE sql STABLE AS $fn$
SELECT CASE WHEN i.leadership_root_issue_id IS NULL AND i.creator_session_id IS NULL AND NOT i.session_semantics
  THEN issue_contract_legacy_basis(i)
  ELSE issue_contract_legacy_basis(i) || jsonb_build_object(
    'requester_session_id', i.creator_session_id,
'requester_leadership', (SELECT jsonb_build_object('root_issue_id',rr.id,'revision',rr.leadership_revision) FROM issue rr WHERE rr.id=i.requester_leadership_root_id AND rr.workspace_id=i.workspace_id),
    'executor', jsonb_build_object('type',i.assignee_type,'id',i.assignee_id) ||
      CASE WHEN i.assignee_type='agent' AND ea.execution_kind='external'
        THEN jsonb_build_object('session_id', eb.session_id) ELSE '{}'::jsonb END,
    'acceptor', jsonb_build_object('type',i.coordinator_type,'id',i.coordinator_id) ||
      CASE WHEN ca.execution_kind='external'
        THEN jsonb_build_object('session_id', cb.session_id) ELSE '{}'::jsonb END,
    'leadership', CASE WHEN r.leadership_squad_id IS NOT NULL THEN
      jsonb_build_object('root_issue_id',r.id,'revision',r.leadership_revision) ELSE NULL END
  ) || CASE WHEN i.assignee_type='squad' THEN jsonb_build_object(
    'executor_representative', (SELECT jsonb_build_object('type','agent','id',a.id) ||
      CASE WHEN a.execution_kind='external' THEN jsonb_build_object('session_id',r.leader_external_session_id) ELSE '{}'::jsonb END
      FROM agent a JOIN squad s ON s.id=i.assignee_id AND s.workspace_id=i.workspace_id
      WHERE a.id=issue_leader_id(i,s.id) AND a.workspace_id=i.workspace_id
        AND a.archived_at IS NULL AND s.archived_at IS NULL
        AND EXISTS (SELECT 1 FROM squad_member sm WHERE sm.squad_id=s.id
          AND sm.member_type='agent' AND sm.member_id=a.id)
        AND (a.execution_kind='managed' OR EXISTS (SELECT 1 FROM external_agent_session es
          WHERE es.id=r.leader_external_session_id AND es.agent_id=a.id
            AND es.workspace_id=i.workspace_id AND es.revoked_at IS NULL)))
  ) ELSE '{}'::jsonb END END
FROM (SELECT 1) seed
LEFT JOIN issue r ON r.id=COALESCE(i.leadership_root_issue_id,i.id) AND r.workspace_id=i.workspace_id
LEFT JOIN agent ea ON ea.id=i.assignee_id AND i.assignee_type='agent' AND ea.workspace_id=i.workspace_id
LEFT JOIN external_agent_session_binding eb ON eb.issue_id=i.id AND eb.agent_id=ea.id AND eb.workspace_id=i.workspace_id AND eb.revoked_at IS NULL
LEFT JOIN agent ca ON ca.id=i.coordinator_id AND i.coordinator_type='agent' AND ca.workspace_id=i.workspace_id
LEFT JOIN external_agent_session_binding cb ON cb.issue_id=i.id AND cb.agent_id=ca.id AND cb.workspace_id=i.workspace_id AND cb.revoked_at IS NULL;
$fn$;

CREATE FUNCTION stamp_actor_session() RETURNS TRIGGER LANGUAGE plpgsql AS $fn$
BEGIN
    NEW.actor_session_id := COALESCE(NULLIF(current_setting('multica.actor_session_id',true),'')::uuid,NEW.actor_session_id);
    RETURN NEW;
END;
$fn$;
CREATE TRIGGER stamp_activity_actor_session BEFORE INSERT ON activity_log
FOR EACH ROW EXECUTE FUNCTION stamp_actor_session();

ALTER TABLE delegation_result_takeover ALTER COLUMN expected_current_return_id DROP NOT NULL;

ALTER TABLE agent_task_queue ADD COLUMN leadership_root_issue_id UUID, ADD COLUMN leadership_revision BIGINT;
ALTER TABLE agent_mailbox_message ADD COLUMN leadership_root_issue_id UUID, ADD COLUMN leadership_revision BIGINT;

CREATE FUNCTION capture_task_leadership() RETURNS TRIGGER LANGUAGE plpgsql SET lock_timeout='3s' AS $fn$
DECLARE r issue;
BEGIN
    IF TG_OP='UPDATE' AND NEW.leadership_root_issue_id IS NOT NULL THEN
        SELECT * INTO r FROM issue WHERE id=NEW.leadership_root_issue_id FOR SHARE;
        IF r.id IS NULL OR r.leadership_revision<>NEW.leadership_revision OR r.leader_agent_id<>NEW.agent_id THEN
            RAISE EXCEPTION 'task leadership changed' USING ERRCODE='40001';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.is_leader_task AND NEW.issue_id IS NOT NULL THEN
        SELECT root.* INTO r FROM issue i JOIN issue root
        ON root.id=COALESCE(i.leadership_root_issue_id,i.id) AND root.workspace_id=i.workspace_id
        WHERE i.id=NEW.issue_id AND root.leadership_squad_id=NEW.squad_id FOR SHARE OF root;
        IF r.id IS NOT NULL THEN
            IF r.leader_agent_id<>NEW.agent_id THEN
                RAISE EXCEPTION 'task leadership changed' USING ERRCODE='40001';
            END IF;
            NEW.leadership_root_issue_id:=r.id;
            NEW.leadership_revision:=r.leadership_revision;
        END IF;
    END IF;
    RETURN NEW;
END;
$fn$;
CREATE TRIGGER capture_task_leadership BEFORE INSERT ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION capture_task_leadership();
CREATE TRIGGER check_task_leadership BEFORE UPDATE OF status ON agent_task_queue
FOR EACH ROW WHEN (NEW.status IN ('dispatched','running')) EXECUTE FUNCTION capture_task_leadership();

CREATE FUNCTION capture_mailbox_leadership() RETURNS TRIGGER LANGUAGE plpgsql SET lock_timeout='3s' AS $fn$
DECLARE r issue;
BEGIN
    IF NEW.kind IN ('issue_assignment','comment_mention','issue_wakeup') AND NEW.issue_id IS NOT NULL THEN
        SELECT root.* INTO r FROM issue i JOIN issue root
        ON root.id=COALESCE(i.leadership_root_issue_id,i.id) AND root.workspace_id=i.workspace_id
        WHERE i.id=NEW.issue_id AND i.workspace_id=NEW.workspace_id
          AND root.leader_agent_id=NEW.recipient_agent_id FOR SHARE OF root;
        IF r.id IS NOT NULL THEN
            IF r.leader_external_session_id IS DISTINCT FROM NEW.target_session_id THEN
                RAISE EXCEPTION 'task leadership changed' USING ERRCODE='40001';
            END IF;
            NEW.leadership_root_issue_id:=r.id;
            NEW.leadership_revision:=r.leadership_revision;
        END IF;
    END IF;
    RETURN NEW;
END;
$fn$;
CREATE TRIGGER capture_mailbox_leadership BEFORE INSERT ON agent_mailbox_message
FOR EACH ROW EXECUTE FUNCTION capture_mailbox_leadership();

CREATE FUNCTION issue_actor_session_conflict(actor_workspace UUID,actor_issue UUID,actor_agent UUID,actor_session UUID) RETURNS BOOLEAN LANGUAGE sql STABLE AS $fn$
SELECT (EXISTS (
 SELECT 1 FROM external_agent_session_binding b
 WHERE b.workspace_id= actor_workspace AND b.issue_id= actor_issue AND b.agent_id= actor_agent
 AND ((b.revoked_at IS NULL AND b.session_id<> actor_session)
      OR (b.revoked_at IS NOT NULL AND b.session_id= actor_session AND NOT EXISTS (
        SELECT 1 FROM external_agent_session_binding active
        WHERE active.workspace_id=b.workspace_id AND active.issue_id=b.issue_id
        AND active.agent_id=b.agent_id AND active.revoked_at IS NULL AND active.session_id= actor_session)))
) OR EXISTS (
 SELECT 1 FROM issue i JOIN issue r ON r.id=COALESCE(i.leadership_root_issue_id,i.id)
  AND r.workspace_id=i.workspace_id
 WHERE i.id= actor_issue AND i.workspace_id= actor_workspace AND r.leader_agent_id= actor_agent
  AND r.leader_external_session_id IS NOT NULL AND r.leader_external_session_id<> actor_session
) OR EXISTS (
SELECT 1 FROM issue i JOIN issue r ON r.id=i.requester_leadership_root_id AND r.workspace_id=i.workspace_id
WHERE i.id= actor_issue AND i.workspace_id= actor_workspace AND i.creator_type='agent' AND i.creator_id= actor_agent
AND (r.leader_agent_id<> actor_agent OR r.leader_external_session_id IS DISTINCT FROM actor_session)
) )::boolean AS conflict;
$fn$;

CREATE FUNCTION guard_task_session_write() RETURNS TRIGGER LANGUAGE plpgsql AS $fn$
DECLARE actor_session_uuid UUID; actor_agent_uuid UUID;
BEGIN
    actor_session_uuid:=NULLIF(current_setting('multica.actor_session_id',true),'')::uuid;
    actor_agent_uuid:=NULLIF(current_setting('multica.actor_id',true),'')::uuid;
    IF actor_session_uuid IS NULL OR actor_agent_uuid IS NULL OR
       (to_jsonb(NEW)-ARRAY['revision','updated_at','last_activity_at'])=
       (to_jsonb(OLD)-ARRAY['revision','updated_at','last_activity_at']) THEN RETURN NEW; END IF;
    IF issue_actor_session_conflict(OLD.workspace_id,OLD.id,actor_agent_uuid,actor_session_uuid)
    THEN RAISE EXCEPTION 'task belongs to another session; explicit handoff required' USING ERRCODE='40001'; END IF;
    RETURN NEW;
END;
$fn$;
CREATE TRIGGER guard_task_session_write BEFORE UPDATE ON issue
FOR EACH ROW EXECUTE FUNCTION guard_task_session_write();

ALTER TABLE issue_wakeup ADD COLUMN target_session_id UUID, ADD COLUMN leadership_root_issue_id UUID, ADD COLUMN leadership_revision BIGINT;

CREATE OR REPLACE FUNCTION capture_comment_wakeup() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c comment; event_type text; source_id uuid; source_agent uuid; payload jsonb; root_id uuid;
BEGIN
 c := CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
 IF NULLIF(current_setting('multica.actor_session_id',true),'') IS NOT NULL AND
    issue_actor_session_conflict(c.workspace_id,c.issue_id,NULLIF(current_setting('multica.actor_id',true),'')::uuid,NULLIF(current_setting('multica.actor_session_id',true),'')::uuid)
 THEN RETURN c; END IF;

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
