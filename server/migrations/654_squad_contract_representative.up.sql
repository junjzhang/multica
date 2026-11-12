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
