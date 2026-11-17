CREATE OR REPLACE FUNCTION guard_issue_contract_completion() RETURNS TRIGGER
LANGUAGE plpgsql AS $fn$
BEGIN
    -- Dedicated policy maintenance must remain possible after delivery. A
    -- changed basis makes the contract stale; revising it reopens the issue.
    IF issue_effective_status(NEW.workspace_id, NEW.status) = 'done'
       AND issue_effective_status(OLD.workspace_id, OLD.status) <> 'done'
       AND EXISTS (SELECT 1 FROM issue_contract c
                   WHERE c.issue_id = NEW.id AND c.workspace_id = NEW.workspace_id
                     AND NOT issue_contract_complete(c.data, issue_contract_basis(NEW))) THEN
        RAISE EXCEPTION 'accept the current contract before completing this issue'
            USING ERRCODE = '23514', CONSTRAINT = 'issue_contract_acceptance_required';
    END IF;
    RETURN NEW;
END;
$fn$;
