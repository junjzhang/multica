CREATE TABLE issue_record (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    seq INTEGER NOT NULL CHECK (seq > 0),
    client_request_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('decision', 'run')),
    summary TEXT NOT NULL,
    rationale TEXT NOT NULL DEFAULT '',
    evidence JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence) = 'array'),
    verdict TEXT NOT NULL CHECK (
        (kind = 'decision' AND verdict IN ('pending', 'keep', 'revert'))
        OR (kind = 'run' AND verdict IN ('pending', 'pass', 'fail', 'inconclusive'))
    ),
    fields JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(fields) = 'object'),
    supersedes_seq INTEGER CHECK (supersedes_seq IS NULL OR (supersedes_seq > 0 AND supersedes_seq < seq)),
    author_type TEXT NOT NULL CHECK (author_type IN ('member', 'agent')),
    author_id UUID NOT NULL,
    actor_session_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
