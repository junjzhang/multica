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

CREATE OR REPLACE FUNCTION issue_contract_complete(data JSONB, basis JSONB) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE AS $fn$
SELECT COALESCE(
    data->'versions'->-1->'basis' = basis
    AND jsonb_array_length(data->'versions'->-1->'criteria') > 0
    AND NOT EXISTS (
        SELECT 1 FROM generate_series(0, jsonb_array_length(data->'versions'->-1->'criteria') - 1) AS idx
        WHERE NOT EXISTS (
            SELECT 1 FROM jsonb_array_elements(data->'decisions') AS d
            WHERE d->>'outcome' = 'accept'
              AND d->'version' = data->'versions'->-1->'number'
              AND d->'criteria' @> jsonb_build_array(idx)
        )
    ), false);
$fn$;

CREATE OR REPLACE FUNCTION guard_issue_contract_completion() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    IF issue_effective_status(NEW.workspace_id, NEW.status) = 'done'
       AND EXISTS (SELECT 1 FROM issue_contract c
                   WHERE c.issue_id = NEW.id AND c.workspace_id = NEW.workspace_id
                     AND NOT issue_contract_complete(c.data, issue_contract_basis(NEW))) THEN
        RAISE EXCEPTION 'accept the current contract before completing this issue'
            USING ERRCODE = '23514', CONSTRAINT = 'issue_contract_acceptance_required';
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER issue_contract_completion_guard BEFORE UPDATE ON issue
FOR EACH ROW EXECUTE FUNCTION guard_issue_contract_completion();
