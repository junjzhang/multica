# Paseo session entry: CLI identity and return address

An existing Paseo conversation connects once and then uses ordinary `multica`
commands under its own external agent identity. Tasks created there record that
agent as author and default coordinator. Executable delegation freezes that
session as its accepted result address; saved results return there through the
existing delivery paths. The coordinator remains issue metadata.

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

`newAPIClient` is the single funnel for commands that act under a session,
including issue, comment, wakeup, repo and runtime-profile commands.

1. **Daemon-managed task wins.** Inside a managed task the task's `mat_` token
   is authoritative. An inherited `PASEO_AGENT_ID` is ignored, so the host
   environment cannot rebind a managed run. An explicit
   `MULTICA_EXTERNAL_SESSION_FILE` inside a managed task is a contradiction and
   is refused rather than resolved.
2. **Paseo session binding.** Otherwise, `PASEO_AGENT_ID` or an explicit file
   selects the binding, and its server, workspace and credential are used. A
   missing, corrupt, expired or mismatched file fails closed with the reconnect
   instruction; it never falls through to the human's default profile.
3. **The credential's own agent must match.** The binding names an agent, but
   the file alone cannot prove the `mea_` token belongs to it. Before any
   command issues a business request, the CLI reads that agent's own mailbox
   (`GET /api/agents/<agent_id>/mailbox`). The auth middleware derives the
   acting agent from the credential, and the handler refuses with 403 when it
   differs from `<agent_id>`; a 200 is the server confirming the token's own
   agent. Reading the agent record is not used for this decision: workspace
   visibility can succeed for a token owned by another agent. A mismatch, a
   rejected credential, or an unreachable server refuses the command before the
   first business request, so nothing is written under the wrong actor.
4. **One workspace for identity and scope.** Commands that resolve a workspace
   besides the credential (`repo`, `runtime profile`) use the already-selected
   client's workspace. They never re-read the human profile after a session
   binding chose the identity, because that would pair an external credential
   with another workspace.
5. **Unchanged human behavior.** With no binding and no managed task, existing
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
  provider, host, session, daemon) or why it could not confirm it.

`valid` is only true after the server confirms the credential's own agent with
the own-mailbox read **and** `GET /api/agents/<agent_id>` returns the same
external agent and provider/host/session/daemon. A file on disk is never
presented as authorization; a 401/403 reports the credential as rejected or as
belonging to another agent, a 404 as a missing agent, and an unreachable server
as unverified. `session status` also applies the same `--profile`,
`MULTICA_TOKEN`, endpoint and workspace conflict rules as ordinary commands, so
it cannot report a binding those commands would refuse.

The command always writes the complete table or JSON report. It exits 0 only
when `status` is `connected` and `valid` is true; `invalid` and `disconnected`
exit non-zero. The token is never part of the output.

`disconnected` means this process has no session environment, or the default
binding file that `PASEO_AGENT_ID` selects does not exist (never connected, or
removed after `/multica disconnect`). A malformed, expired or mismatched file is
`invalid`. Inside a managed task, status explains that the task credential is
authoritative instead.

## Coordinator metadata and result return

Claim responses carry optional `coordinator_type` / `coordinator_id` from the
issue row, including on resumed turns. The daemon presents them as
`[Issue coordinator]` metadata, not as a delegation return address.

Runs with accepted delegation return addresses have their saved results routed
to those frozen addresses automatically. Changing the issue coordinator does
not retarget existing obligations. Agents must not infer a return address from
the coordinator, assignee, trigger author or latest commenter, and must not add
a coordinator mention merely to duplicate the result return.

Missing/unknown coordinator identities and self-coordination omit this metadata
block. Neither coordinator metadata nor task completion proves that a recipient
received or accepted a result.

## Out of scope

- The plugin commands (`/multica connect`, `/multica status`, `/multica disconnect`)
  and any operator configuration.
- WORK-33/34 workflow context protocol, watchdogs, role engines, recovery.
- Any replacement session, provider reload, or second result queue.
