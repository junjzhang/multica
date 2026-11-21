# Issue records

An issue record is an append-only, structured entry in an issue's ledger. It
captures one step of long-running work that a reviewer needs to audit later:

- `decision`: a choice made while working, such as a hypothesis tried in an
  optimization loop, a rejected alternative, or a rollback.
- `run`: one execution of an experiment or validation job, with its protocol,
  inputs, platform job, conclusion and artifact location.

Both have the same shape: what was done, why, where the evidence is, and the
verdict. Comments remain for conversation. Records do not notify anyone and do
not start agent runs.

## Data

`issue_record` (no foreign keys; cleaned up with the issue and workspace):

| Column | Meaning |
| --- | --- |
| `id` | UUID primary key |
| `workspace_id`, `issue_id` | Owning issue |
| `seq` | 1-based position within the issue; unique per issue |
| `client_request_id` | Caller UUID; makes retries idempotent; unique per issue |
| `kind` | `decision` or `run` |
| `summary` | One line, at most 500 characters |
| `rationale` | Why, at most 4,000 characters; optional |
| `evidence` | JSON array of at most 20 strings (URLs, commits, job IDs, paths), each at most 2,000 characters |
| `verdict` | See below |
| `fields` | Flat JSON object of primitive values, at most 4 KiB; run inputs such as protocol version, image digest or job ID |
| `supersedes_seq` | Earlier record this one corrects or concludes; optional |
| `author_type`, `author_id`, `actor_session_id` | Authenticated writer |
| `created_at` | Server time |

Verdicts:

- `decision`: `pending`, `keep`, `revert`
- `run`: `pending`, `pass`, `fail`, `inconclusive`

There is no update or delete operation. A correction, or the conclusion of a
`pending` run, is a new record whose `supersedes_seq` names the earlier one.
`supersedes_seq` must refer to an existing record of the same kind on the same
issue. A record is current when no later record supersedes it; readers derive
this, it is not stored.

An issue holds at most 5,000 records.

## API

`GET /api/issues/{id}/records` returns `{ "records": [...] }` ordered by `seq`.
An optional `kind` query parameter filters by kind.

`POST /api/issues/{id}/records` appends one record:

```json
{
  "client_request_id": "01900000-0000-7000-8000-000000000001",
  "kind": "run",
  "summary": "80B GRPO trainer-only FA3 dQ repeat",
  "rationale": "Check whether 3-block accumulation reorders FP32 sums",
  "evidence": ["ci-run/0817b8c4c486", "job-instance/799791934"],
  "verdict": "pending",
  "fields": { "protocol": "v1", "image": "sha256:dd88…" },
  "supersedes_seq": null
}
```

The response is the stored record (201, or 200 when replaying). Reusing a
`client_request_id` on the same issue returns the existing record when kind,
summary, verdict and supersedes_seq match, and 409 otherwise. Any member or
agent that can read the issue can append. The server publishes
`issue_record:created` so open clients refresh; nothing enters the inbox.

## CLI

```bash
multica issue record add WORK-1 --kind decision --summary "Tile 128x64" \
  --why "Halves smem bank conflicts" --evidence <commit> --verdict keep
multica issue record add WORK-1 --kind run --summary "..." --verdict pending \
  --field protocol=v1 --field image=sha256:...
multica issue record add WORK-1 --kind run --summary "Concluded" \
  --verdict pass --supersedes 3
multica issue record list WORK-1 [--kind run] [--output table|json|tsv]
```

`add` generates a request UUID unless `--request-id` is given and prints it,
so an uncertain write can be retried with the same UUID. `--output tsv` writes
one row per record for terminal tools and spreadsheets.

## Interface

Issue details show a Records section when the issue has records: a compact
table of seq, kind, summary, verdict, evidence links and author. Superseded rows
are de-emphasized and link to the record that supersedes them.
