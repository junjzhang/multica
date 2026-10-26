# Paseo Multica

A private Paseo directory plugin that binds the current Paseo conversation to
Multica as its own external agent. It is intentionally smaller than a task
board: three composer commands, one read-only status panel, and one operator
settings screen.

```text
/multica connect      register this conversation, mint its external credential
/multica status       show the binding, return address, and local validity
/multica disconnect   revoke this binding's credential and remove local context
```

## What it reuses

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
credential against the agent's own mailbox endpoint before trusting it. Expired
or revoked credentials are repaired only by explicit `/multica connect`. A
binding for another server, workspace, host, or daemon is reported and left
untouched rather than silently rebound.

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

```bash
pnpm --filter @multica/paseo-multica typecheck
pnpm --filter @multica/paseo-multica test
```

Tests use a temporary `HOME`, a fake host adapter, and a fake Multica API; the
HTTP helper tests use a loopback server. They do not read a real user's
Multica configuration or contact a production server.

The plugin targets the deployed Paseo SDK/API `0.13.0-beta.1` (revision
`fddb9e3a`). It is a directory plugin: install it from Settings → Plugins or
with `paseo plugin install <absolute path>` on the daemon host.

## Limits

- `status` reports whether the local binding is complete, host-compatible, and
  unexpired; it does not poll the server. A credential revoked elsewhere is
  discovered on the next explicit `connect` or by the Multica command that uses
  it.
- No automatic enrollment of existing sessions, no automatic model messages, no
  MCP server, and no provider reload.
