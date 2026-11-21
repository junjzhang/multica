-- name: LockIssueRecordLedger :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('issue_id')::uuid::text || ':issue_record', 0));

-- name: GetIssueRecordByRequest :one
SELECT * FROM issue_record
WHERE workspace_id = @workspace_id AND issue_id = @issue_id AND client_request_id = @client_request_id;

-- name: GetIssueRecordBySeq :one
SELECT * FROM issue_record
WHERE workspace_id = @workspace_id AND issue_id = @issue_id AND seq = @seq;

-- name: NextIssueRecordSeq :one
SELECT (COALESCE(MAX(seq), 0) + 1)::int AS next_seq FROM issue_record
WHERE workspace_id = @workspace_id AND issue_id = @issue_id;

-- name: CreateIssueRecord :one
INSERT INTO issue_record (
    workspace_id, issue_id, seq, client_request_id, kind, summary, rationale,
    evidence, verdict, fields, supersedes_seq, author_type, author_id, actor_session_id
) VALUES (
    @workspace_id, @issue_id, @seq, @client_request_id, @kind, @summary, @rationale,
    @evidence, @verdict, @fields, sqlc.narg('supersedes_seq'), @author_type, @author_id,
    sqlc.narg('actor_session_id')
)
RETURNING *;

-- name: ListIssueRecords :many
SELECT * FROM issue_record
WHERE workspace_id = @workspace_id AND issue_id = @issue_id
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
ORDER BY seq;
