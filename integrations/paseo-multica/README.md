# Paseo Multica

A private Paseo directory plugin that binds the current Paseo conversation to
Multica as its own external agent. It is intentionally smaller than a task
board: three composer commands, one read-only status panel, and one operator
settings screen.

```text
/multica connect      register this conversation, mint its external credential
/multica status       show the binding, return address, and current validity
/multica disconnect   revoke this binding's credential and remove local context
```

## What it reuses

Agents can explicitly connect their current conversation from a shell:

```text
multica session connect --output json
multica session status --output json
```

This requires a Paseo CLI with `plugin rpc` and this plugin enabled on the
selected host. The CLI uses `PASEO_AGENT_ID` and the existing Paseo daemon
target, invokes `session.connect`, then verifies the credential's own agent
and registered return address with Multica. A repeat connection reuses the
plugin's existing transaction and credential. No provider restart is needed.

The plugin settings select the Multica server and workspace. Remove explicit
`--profile`, `--server-url`, `--workspace-id`, `MULTICA_TOKEN`,
`MULTICA_SERVER_URL`, and `MULTICA_WORKSPACE_ID` overrides before connecting.
Managed task processes cannot connect an external session. A custom
`MULTICA_EXTERNAL_SESSION_FILE` must name this session's default binding path.

- The Multica external-agent registration and token endpoints. Registration is
  keyed by `provider/host/session`, so a repeat connect returns the same
  identity instead of creating a second agent.
- The Paseo host records. The plugin asks the daemon that actually runs it for
  `self.serverId`; the composer supplies the agent id, which is refreshed
  against that same daemon and rejected when archived.
- The existing Multica mailbox/delivery daemon. The configured `daemon_id` is
  the authorized delivery daemon; this plugin adds no queue, scheduler, MCP
  server, provider reload, or send fallback.

## Operator configuration

Settings → Plugins → Paseo Multica:

| Setting | Meaning |
| --- | --- |
| Owner profile | Multica CLI profile name on the daemon host. Empty means the default profile (`~/.multica/config.json`); a named profile reads `~/.multica/profiles/<name>/config.json`. |
| Delivery daemon | Multica daemon id that is allowed to deliver results to this host. |

The backend reads the owner profile only to register the external agent, mint
its `mea_` credential, and revoke it on disconnect. The owner credential is
never written to the shared file, shown in the client, logged, sent in an RPC
result, or injected into provider environment.

## Shared file v1

`~/.multica/external-sessions/paseo/<canonical-paseo-agent-id>.json`, directory
`0700`, file `0600`, atomic replacement through a synced temporary file and
rename. `MULTICA_EXTERNAL_SESSION_FILE` is a CLI-side override; the plugin
always writes this default path so an already-running conversation can find it
through its existing `PASEO_AGENT_ID` without a provider reload.

The file is a private credential cache, not task/queue authority. A repeat
connect reuses a locally valid, host-compatible binding; it verifies that
credential against the bound agent's own mailbox endpoint before trusting it.
That endpoint authorizes the credential's subject rather than a merely visible
agent, so another agent's credential cannot validate this binding. Expired or
revoked credentials are repaired only by explicit `/multica connect`. A binding
for another server, workspace, host, or daemon is reported and left untouched
rather than silently rebound.

Business CLI requests use the file's credential, workspace and exact session
as one identity: after the own-mailbox check, `X-Return-Session` carries that
session. Conflicting return-session flags or environment values are rejected.
Managed tasks keep their `mat_` identity and cannot replace it with an external
file; explicit owner registration/credential commands retain their separate
owner entry point. A disconnected Paseo business command does not fall back
to a human profile.

`disconnect` revokes only the token recorded in this session's file, then
deletes that file. If the configured owner profile cannot revoke that token —
because it was issued by another server or workspace — the file is kept and the
error names the agent, token, server, and workspace so the original profile can
be restored before retrying. It never archives or deletes the Paseo
conversation, its history, or any other agent's binding.

`agent.session_open` injects only `MULTICA_EXTERNAL_SESSION_FILE` for an
interactive opening of a session that already has a valid local binding. It
skips history replay and never injects an owner credential.

## Development

This plugin is its own pnpm project with its own lockfile. It is loaded by
Paseo and is not part of the Multica web/mobile dependency graph, so installing
or changing it does not touch the repository lockfile.

```bash
cd integrations/paseo-multica
pnpm install --frozen-lockfile
pnpm typecheck
pnpm test
```

Tests use a temporary `HOME`, a fake host adapter, and a fake Multica API; the
HTTP helper tests use a loopback server. They do not read a real user's
Multica configuration or contact a production server.

`.github/workflows/paseo-multica.yml` runs the same frozen install, typecheck,
and package tests, but only for changes inside this directory or to that
workflow.

The plugin targets the deployed Paseo SDK/API `0.13.0-beta.1` (revision
`fddb9e3a`). It is a directory plugin: install it from Settings → Plugins or
with `paseo plugin install <absolute path>` on the daemon host.

## Limits

- `status` checks that the local binding is complete, host-compatible, and
  unexpired, then asks the bound agent's mailbox whether Multica still accepts
  the credential. A credential revoked elsewhere therefore shows as needing
  attention. If that request cannot complete, the panel reports the binding as
  unverified instead of claiming a connection. `status` never repairs anything;
  `/multica connect` does that explicitly.
- No automatic enrollment of existing sessions, no automatic model messages, no
  MCP server, and no provider reload.
