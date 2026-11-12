DROP TRIGGER IF EXISTS issue_contract_completion_guard ON issue;
DROP FUNCTION IF EXISTS guard_issue_contract_completion();
DROP FUNCTION IF EXISTS issue_contract_complete(JSONB, JSONB);
DROP FUNCTION IF EXISTS issue_contract_basis(issue);
