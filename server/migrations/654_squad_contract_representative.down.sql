CREATE OR REPLACE FUNCTION issue_contract_basis(i issue) RETURNS JSONB
LANGUAGE sql STABLE AS $fn$
SELECT jsonb_build_object(
    'executor', jsonb_build_object('type', i.assignee_type, 'id', i.assignee_id),
    'acceptor', jsonb_build_object('type', i.coordinator_type, 'id', i.coordinator_id),
    'merge_authorization', jsonb_build_object('allowed', i.merge_allowed,
        'repository_url', i.merge_repository_url, 'target_branch', i.merge_target_branch,
        'granted_by', i.merge_granted_by, 'granted_at', i.merge_granted_at),
    'criteria', i.acceptance_criteria
);
$fn$;
