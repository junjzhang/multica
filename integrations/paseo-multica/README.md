# Paseo Multica

A private Paseo directory plugin that binds the current Paseo conversation to
Multica as its own external agent. It embeds the Multica web app and provides three composer commands, a task
summary pill, a connection panel, a delivery card, and an operator settings
screen.

```text
/multica connect      register this conversation, mint its external credential
/multica status       show the binding, return address, and current validity
/multica disconnect   revoke this binding's credential and remove local context
```

## Multica web page

Open it from the **Multica** sidebar entry, the Command Center ("Open
Multica"), or as a **Multica** panel in a workspace or the Explorer. Every
entry shows the same page; the plugin does not reimplement Multica's UI.
Task and delivery cards open task details in one Explorer panel. The panel follows
the selected task and offers a back action for recently opened tasks. Its state
is restored by Paseo. The browser action remains available on each card. Task
links must point to an issue in the configured workspace.

- **Desktop** uses Paseo's sandboxed browser profile (`persist:paseo-browser`)
  in a `<webview>`, the only webview Paseo's main process allows to attach.
- **Browser** clients use an iframe.
- **Mobile** offers "Open in browser", which opens `externalUrl` (default:
  `url`) without the sign-in key; the browser signs in with the proxy
  password. Use it when the embedded origin is unreachable from a phone.

After the first open, the main page stays resident for the renderer's lifetime.
Closing the panel or leaving the screen only parks it off screen; reopening
reuses the same page, navigation, and form state. An explicit retry replaces
only a failed page. Task pages are released when their panels unmount.

### Sign-in

The page is served behind a reverse proxy that requires a password. A webview
cannot answer a browser password prompt, so the proxy also accepts a session
cookie issued by a one-time sign-in link:

1. The plugin backend reads `~/.config/multica/paseo-web.json` on the daemon
   host and returns `<url>/paseo-login?k=<key>` through the authenticated
   plugin RPC. The key never appears in settings, snapshots, or logs.
2. The proxy checks the key, sets an HTTP-only, `Secure`, `SameSite=Lax`
   cookie for 30 days, and redirects to the board, so the key does not stay in
   the page URL.
3. Paseo's persistent browser profile keeps the cookie across restarts.

```json
{
  "url": "https://multica.example",
  "key": "<at least 32 URL-safe characters>",
  "externalUrl": "http://10.0.0.5:3210"
}
```

`url` and the optional `externalUrl` must be bare HTTP(S) origins: no
credentials, path, query, or fragment.
Keep the file `0600`. The proxy must hold the same key; rotating it means
updating both, after which the next open signs in again.

## Delivery card

A mailbox delivery reaches the session as one ordinary user message. The
plugin registers a timeline transformer for user messages and replaces a
recognized Multica delivery with a card: the task identifier and title, the
delivery kind, and a link to the task. The delivered prompt is never
rewritten, and stays available under the card's raw details together with the
mailbox, issue, and comment IDs.

The daemon renders the readable part of that prompt from optional
`issue_identifier`, `issue_title`, and `issue_url` payload fields. Anything
this plugin does not fully recognize therefore renders exactly as it arrived:
a message from a daemon that predates those fields, a message that is still
streaming, a prompt missing one of its stable IDs, and ordinary conversation.

## Session tasks

Every Paseo agent on the host gets one Multica composer pill. For a connected
session the pill prioritizes deliveries awaiting acceptance and blocked owned
work, then shows the number of tasks in progress. The panel lists:

- needs attention: undecided deliveries, unanswered executor mentions, and
  blocked owned tasks;
- delegated: open issues dispatched through this session's accepted return
  address, including those whose result has already returned;
- owned: issues the session's agent executes or leads as task leader.

Connection details are collapsed below the task lists. Older servers that do
not supply delegated items fall back to pending returns.

A notification-only `mention://agent-notify/<uuid>` wakes the agent without adding a result return and is not listed as an unanswered request.

An unanswered executor mention is an explicit agent mention link in a comment
by the issue's current executor on an open issue, with no later comment by this
agent in the same unresolved thread. Mailbox receipts are not used as replies. For a
blocked delegated issue, the panel labels the first line of the current
executor's latest ordinary comment as its source; no reason is inferred when
there is no comment.

The data comes from the read-only `GET /api/agents/{id}/session-tasks`
projection, scoped to the credential's own agent and requiring a session-bound
`mea_` credential. Owned and acceptance lists are agent-scoped because an
external agent registers one session per conversation; a multi-session agent
would need a session-scoped projection. Everything outside that happy path degrades to an
explanation instead of an error: a session with no local binding, an unreadable
context, an expired or revoked credential, a legacy token whose agent has no
registered session, a server that does not expose the projection, and a
network failure all keep the composer usable and leave the counts off.

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
revoked credentials are repaired only by explicit `multica session connect`
(or `/multica connect` in the composer). A binding
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
  `multica session connect` (or `/multica connect` in the composer) does that explicitly.
- No automatic enrollment of existing sessions, no automatic model messages, no
  MCP server, and no provider reload.

## Background waits

The server plugin contributes Multica waits to Paseo's existing background-task
pill. It reconciles immediately and about every five seconds even with the app
closed. This requires the Paseo host's external background source extension and
a Multica daemon with `wait list`; old hosts show an upgrade notice in plugin
settings.

Set **Local wait daemon profile** explicitly in the connection settings. Enter
`default` for `~/.multica`, or the named profile owning the waits. Its endpoint
must match **Delivery daemon**. The local endpoint secret and owner credential
stay in the server process. The owner profile supplies read access to frozen
registered session addresses; a provider session pin does not establish a
Paseo address.

Ended waits remain visible until Multica reports `handled`. A failed read retains
the last snapshot as stale and disables cancellation. **Cancel local wait** stops
the local monitor only; remote CI/GPU jobs continue. Plugin unload removes the
projection without stopping waits. Reload and daemon restart reconstruct it from
Multica; a wait that Multica marks lost after restart is shown as lost.
