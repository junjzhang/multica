# Paseo Multica

A private Paseo directory plugin that binds the current Paseo conversation to
a Multica external Agent with a session credential. Conversations on one host can
share that Agent. It embeds the Multica web app and provides three composer commands, a task
summary pill, a connection panel, a delivery card, and an operator settings
screen.

```text
/multica connect      register this conversation, mint its external credential
/multica status       show the binding, return address, and current validity
/multica disconnect   revoke this binding's credential and remove local context
```

## Multica web page

The **Multica board** sidebar entry and **Open Multica board** command open the
global web app. The right-side **This session’s tasks** panel shows the current
conversation’s task summary and connection. Task and delivery cards open issue
details in an Explorer panel with Back history scoped to the host, workspace
and conversation. Paseo restores that scoped history. After upgrading from the
workspace-context panels, close old Multica/detail tabs and reopen them from
the conversation; their unscoped history is discarded.
The browser action remains available on each card. Task
links must point to an issue in the configured workspace.

- **Desktop** uses Paseo's sandboxed browser profile (`persist:paseo-browser`)
  in a `<webview>`, the only webview Paseo's main process allows to attach.
- **Browser** clients use an iframe.
- **Mobile** offers "Open in browser", which opens `externalUrl` (default:
  `url`) without the sign-in key; the browser signs in with the proxy
  password. Use it when the embedded origin is unreachable from a phone.

After the first open, the global board stays resident for the renderer's lifetime.
Leaving its screen only parks it off screen; reopening
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

Every Paseo agent on the host gets one Multica composer pill. It names the
session's most urgent task tree and counts its to-dos on one line, for example
`WORK-494 · 待办 3`, and opens the panel. It reads `无待办`, `无任务`, `未连接`
or `不可用` when there is nothing to do, no task, no binding, or no answer.
When that tree has subtasks, the pill's icon becomes a static ring of its
closed/total children and the tooltip adds `已完成 2/5 个子任务` under the task
name, which is clipped to one line.

The panel has two parts:

- **待办** (to-do queue): MRs waiting for your approval, tasks whose turn is
  yours or this session's (usually an acceptance), and anomalies. The scope
  switch changes only the tree roots, never what counts as a to-do. The row opens
  the task; a to-do about an MR (approval, unmerged, conflicting, failed CI) ends
  in an MR chip that opens that MR. A row whose task runs in another Paseo session
  ends in a session target that jumps to that session instead.
- **需要你** holds only what the user alone can clear: an MR waiting for their
  approval, a contract pending the user, and a delivery that failed because its
  session is archived. **待本会话处理** holds everything this session must do:
  acceptance, its own pending turn, an unmerged MR of a closed task, and an
  anomaly. The to-do kinds are unchanged; only the two blocks and their titles
  differ. The pill counts both, since both are still open work.
- **Task trees**, split into 需要处理 / 进行中 / 搁置, with 搁置 folded. Finished
  work is left out: a closed tree is not listed, and a closed child with no open
  MR counts only in its parent's progress. Every task row leads with Multica's
  status glyph (dotted backlog, ring todo, half in progress, three quarters in
  review, slash blocked), colored amber, green or red for in progress, in review
  and blocked. A row's MR chip gathers the open MRs of its whole subtree, each
  MR once even when several tasks link it: a single MR's number and state, or a
  count for several, colored by the most urgent one
  (red for failed CI or a conflict, amber when one waits for your approval). A
  single MR opens directly; several expand in place, each naming the subtask it
  came from. Merged MRs appear on the rows that delivered them: an open task's
  merged MRs, under its disclosure, and a finished batch, which lists up to three with a count for the
  rest instead of a `done/total` count. A single-child parent shows no count. Children are grouped by
  `stage` into batches.

**本会话** roots the trees at tasks this session executes, leads, accepts or
delegated. **全部** roots them at every open task whose parent is not open, and
groups them by Multica Project. Both add closed tasks that still have an open MR
(for 本会话, those the session executes, accepts or coordinates), because
acceptance usually closes the task while its MR still waits for approval.

Classification lives in `shared/board.ts` as a pure function of issue facts and
the clock. It is display-only: it never writes a task or changes a status.

```text
需要处理: contract.pending_actor is this session or the user
          or an MR waits for the user: contract accepted, MR open, user is a reviewer and has not approved
          or a done task still has an open MR (未合并 / 有冲突 / CI 失败); a cancelled task is always closed
          or an anomaly: blocked, failed CI on an open MR, or a failed latest run, with no active run
进行中:   an active run (queued, dispatched, running, waiting_local_directory, deferred)
          or the executor's turn, not backlog, last_activity_at within STALE_AFTER_DAYS
搁置:     every other open task, counted as Backlog, 7 天无动静, or 其他 when still fresh
no contract: in_review is this session's acceptance only when its coordinator is bound
             to this session; every other task counts as the executor's turn
STALE_AFTER_DAYS = 7
```

A tree goes to the section of its most urgent task, and is closed only when
every task in it is done and no MR in it is open.

"The user" is the bound external agent's owner. MR approval needs the user's
forge usernames in the plugin settings (`forgeUsernames`, e.g. `name, github-name`);
without them no MR counts as waiting for the user; TGit reports a reviewer who has not
approved as `review_state: "approving"` and one who has as `"approved"`.

The plugin uses its configured owner profile to build the board:
`GET /api/agents/{id}/session-tasks?session_id=…` for this session's roots, `open_only`
issues for 全部, batched `children` for the subtrees, and per open task its
issue (contract, coordinator session binding and `last_activity_at`),
`task-runs` and, when linked, `pull-requests`. The workspace MR summary finds
closed tasks with open MRs. A reconnect or disconnect drops the cached board. A board stops at 300 tasks and says so. The pill and panel
share a 15-second cache; the panel's ⟳ bypasses it.

A session without an identity on Multica has an empty session board. The
workspace board remains available. Owner authorization and network failures
show an explanation in the panel. Failed deliveries to archived sessions show
under **需要你** and open the archived session in Paseo.

A row's session target appears only when the host exposes
`navigation.openAgent` (absent on older hosts and in the browser), the task's
executor is an external agent with a deliverable Paseo binding, that binding is
not the session showing the board, and the daemon still lists the session
unarchived. It calls `openAgent({ serverId, agentId })` with the binding's host
and session ids and never opens the task.

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
| Your forge usernames | Your own GitHub or TGit usernames, comma separated. An open MR of an accepted task that still waits for one of them becomes a to-do. |

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

The file is a private credential cache. The CLI asks the plugin to issue a
one-hour credential on first use and to renew it once five minutes or less
remain. A cached credential with more time left skips the RPC. The plugin
refuses issuance while the conversation is archived or missing. Run
`multica session connect` or `/multica connect` to repair a revoked credential
before its scheduled renewal. A binding
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

## Embedded appearance

The plugin passes the renderer's resolved `PluginTheme.colorScheme` to each resident
page. Update the Paseo desktop renderer as well as the plugin and Multica web app;
a daemon update alone cannot expose the client field. Older clients keep page access
and show an update notice. After publishing the web app and plugin, refresh the
desktop renderer to load the new guest application. A plugin reload alone retains
resident pages running the previous web bundle.

The web app uses a temporary next-themes `forcedTheme`. Theme changes do not call
`setTheme`, write storage, change `src` or replace a resident page. The iframe's
browsing-context name binds its parent origin and latest appearance across same-origin
login redirects and route reloads. Accepted theme messages update the guest's actual
`window.name`; changing the iframe element's name alone does not update that value.
Each mounted document requests the current theme after registering its receiver,
because `load` can occur before application hydration. Login 302 and language-change reloads discard route
query parameters, so they cannot own this binding. Webviews receive initial URL state
and later the fixed script channel. Browser messages use
the configured guest origin, and the receiver checks the exact parent window and
origin. Electron sends only a validated light/dark value through a fixed script
restricted to the configured guest origin, after `dom-ready`. Guest navigation clears
that readiness gate. Partition and sign-in behavior are unchanged.

`e2e/paseo-theme.spec.ts` uses the production resident component, web theme provider,
SDK mapper and UI tokens in isolated fixtures. It checks opposite OS/host settings,
default login redirects, route reloads, live changes, drafts, scrolling, reopening,
multiple hosts and untouched standalone
storage and forced-theme toast appearance. OS preferences are emulated through
Chromium media emulation/CDP; the host system setting is unchanged. The Electron case uses a real webview with `persist:paseo-browser` inside a
temporary user-data directory. These are bridge integration fixtures, not a login or
full-application journey. iOS/Android are not covered.

Run from the repository root with `PASEO_THEME_HOST_CHECKOUT` pointing to the SDK
checkout and `PLAYWRIGHT_BROWSER_EXECUTABLE` pointing to Chromium. For Electron, set
`PASEO_THEME_ELECTRON` and run under an available display (for example `xvfb-run -a`):

```bash
pnpm exec playwright test e2e/paseo-theme.spec.ts
```

Before/after evidence is in [the theme capture directory](../../docs/assets/work398-theme).
The `iframe-*` and `electron-*` files identify the actual embedding path. Captures
labelled `before` use the legacy bridge with no host appearance field.

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

## Session task scope

The right-side Multica panel shows **This session’s tasks**. **Multica board** in
the sidebar and command center opens the global board. Task details and their
Back history belong to one host, workspace and Paseo session (`agentId` in the
Paseo SDK), even when sessions share a workspace and Multica external Agent.
Saved history without that identity is discarded; production tasks are unchanged.

The server uses active issue/session bindings for executors, the root's leader
session for leadership, the current contract acceptor session for acceptance,
and frozen Mailbox targets for requests. Delegation lists retain their accepted
return sessions. Agent-only historical work is visible on the global board and
is not guessed into a session. A reply resolves a request in this projection
only when its actor session matches the request target; Mailbox ACK is unchanged.

Run `pnpm test:browser` for the Chromium regression. It mounts production task,
connection and detail components with a small DOM adapter and fixed RPC data;
it checks same-host/workspace switching, independent detail history, empty and
disconnected states. Screenshots go to `tests/browser/artifacts` (override with
`BROWSER_EVIDENCE_DIR`); `PLAYWRIGHT_BROWSER_EXECUTABLE` selects an installed
Chromium. Install Chromium with `pnpm exec playwright install chromium` if needed.
This is a component integration fixture, not an installed Paseo/Electron,
production login, native mobile, or end-to-end API test. Existing resident-page
security/theme tests and DB-backed handler regressions cover those respective
boundaries without changing delivery routes or credentials.
