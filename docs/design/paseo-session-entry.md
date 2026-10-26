# Paseo session entry: CLI identity and return address

An existing Paseo conversation connects once and then uses ordinary `multica`
commands under its own external agent identity. Tasks created there record that
agent as author and default coordinator, and a managed run is told the issue's
recorded result recipient so an explicit handoff returns to the same
conversation through the existing mailbox.

This document is the minimal contract for the CLI half. The Paseo plugin that
writes the binding, and the coordinator's integration/deployment, are separate
slices. Nothing here adds a queue, a `return_to` database state, a replacement
session, or a managed run for an external recipient.

## Why the file exists

External registration and delivery already work (`docs/design/external-agent-mailbox.md`).
What was missing was a way for a *fresh* CLI process inside an already-running
Paseo session to discover that identity without a provider reload. Paseo exports
`PASEO_AGENT_ID` in every provider launch, so an existing session can read a
binding written later, on its next command.

The binding is a private credential cache, not authority. Authority remains the
`mea_` token plus the server's checks (owner membership, workspace, live
external agent, revocation, expiry) on every request.

## Shared file contract v1

Default path, on the provider execution host:

```
~/.multica/external-sessions/paseo/<PASEO_AGENT_ID>.json
```

`MULTICA_EXTERNAL_SESSION_FILE` optionally selects an explicit absolute path.

```json
{
  "version": 1,
  "provider": "paseo",
  "host_id": "verified-paseo-server-id",
  "session_id": "canonical-paseo-agent-uuid",
  "agent_id": "multica-external-agent-uuid",
  "daemon_id": "authorized-multica-delivery-daemon-id",
  "server_url": "configured-multica-api-base",
  "app_url": "configured-multica-browser-base",
  "workspace_id": "multica-workspace-uuid",
  "token": "mea_example-not-a-real-token",
  "token_id": "issued-credential-uuid",
  "expires_at": "RFC3339"
}
```

- The plugin is the only writer; it replaces the file atomically and keeps the
  directory `0700` and the file `0600`.
- The CLI is a reader. It never rewrites the file, copies its token elsewhere,
  or prints the token.
- A context is rejected when: `version`/`provider` are not v1/paseo; a required
  field is missing; an id is not a plain identifier; `session_id` disagrees with
  `PASEO_AGENT_ID`; `server_url` is not an http(s) base URL; the token does not
  start with `mea_`; `expires_at` is not RFC3339 or is in the past; or the file
  is readable by group/other. Each failure names the field and tells the user to
  reconnect.

## Resolution and precedence

`newAPIClient` is the single funnel for issue, comment and wakeup commands.

1. **Daemon-managed task wins.** Inside a managed task the task's `mat_` token
   is authoritative. An inherited `PASEO_AGENT_ID` is ignored, so the host
   environment cannot rebind a managed run. An explicit
   `MULTICA_EXTERNAL_SESSION_FILE` inside a managed task is a contradiction and
   is refused rather than resolved.
2. **Paseo session binding.** Otherwise, `PASEO_AGENT_ID` or an explicit file
   selects the binding, and its server, workspace and credential are used. A
   missing, corrupt, expired or mismatched file fails closed with the reconnect
   instruction; it never falls through to the human's default profile.
3. **Unchanged human behavior.** With no binding and no managed task, existing
   profile resolution is untouched.

Explicit input that disagrees with the binding is rejected, never silently
preferred:

| Input | Rule |
| --- | --- |
| `--profile` | rejected; a profile is a credential/server/workspace bundle the session already owns |
| `MULTICA_TOKEN` | rejected; the session's `mea_` credential is the only identity |
| `--server-url` / `MULTICA_SERVER_URL` | allowed only when it normalizes to the binding's `server_url` |
| `--workspace-id` / `MULTICA_WORKSPACE_ID` | allowed only when it equals the binding's `workspace_id` |

A session request carries the `mea_` token and the binding's workspace. It does
not send `X-Agent-ID`/`X-Task-ID`: an external session is authorship, not a
managed task, and no synthetic task id may enter attribution.

## `multica session status`

`multica session status --output json` reports inspection state without secrets:

- `status`: `connected`, `invalid` or `disconnected`, plus `valid`.
- `context_file` and `explicit_context_file`, so the user knows which file was
  read.
- Non-secret identity: provider, host, session, agent, daemon, server, app URL,
  workspace, token id, expiry.
- `return_address`: the existing conversation's `paseo://h/<host>/agent/<session>`
  and the Multica daemon that delivers to it.
- `remote`: the server's answer about the binding (agent name, execution kind,
  bound session) or why it could not confirm it.

`valid` is only true after `GET /api/agents/<agent_id>` succeeds **and** returns
the same external agent, host and session. A file on disk is never presented as
authorization; a 401/403 reports the credential as rejected, a 404 as a missing
agent, and an unreachable server as unverified. The command exits 0 for every
binding state — the state is the report — and only a malformed invocation is an
error. The token is never part of the output.

`disconnected` means this process has no session environment. Inside a managed
task, status explains that the task credential is authoritative instead.

## Result return address

The issue coordinator is the recorded result recipient (no new DB state):

- Claim responses gain optional `coordinator_type` / `coordinator_id`, filled
  from the issue row at claim time. Missing type or id stays empty.
- Every claim, including one that resumes a prior session, carries the current
  value, so a coordinator changed between runs reaches the next run.
- The daemon renders a per-turn `[Result return address]` block:
  - `agent` recipient: an explicit `mention://agent/<id>` target is the only
    handoff that reaches it; nothing returns results automatically.
  - `member` recipient: members have no mailbox to wake; report in the issue
    thread and do not claim they were notified.
  - no recipient, unknown actor kind, or an old server that never sent the
    fields: no block. The run does not infer a recipient from the assignee, the
    trigger author or the latest commenter.
  - the agent coordinator is the executing agent itself (an agent-created issue
    assigned back to its creator): no block either. Naming the run's own agent
    would tell it to mention itself, and a self-mention queues another run while
    this one is still executing, so the run would keep waking itself.
- The block states the permission boundary: the coordinator is this issue's
  recorded recipient, not evidence of who delegated this or any other task.

This keeps the fixed-coordinator concept from growing into a general delegator
model: it is a recorded return address and a wakeability fact, nothing more.

## Out of scope

- The plugin commands (`/multica connect`, `/multica status`, `/multica disconnect`)
  and any operator configuration.
- WORK-33/34 workflow context protocol, watchdogs, role engines, recovery.
- Any replacement session, provider reload, or second result queue.
