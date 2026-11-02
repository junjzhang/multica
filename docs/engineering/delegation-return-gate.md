# Delegation return gate

This slice implements the transactional delegation half of
[task-session-delivery.md](../design/task-session-delivery.md): an Agent that
hands work to another executable Agent or squad must name the address its result
returns to, and that address is accepted before any child work exists. Result
submission, notification delivery, takeover UI, and the maintenance-window
cutover remain later slices.

## Accepted returns

`issue_delegation_return` records one accepted relationship per
`(workspace, issue, dispatcher agent)` key (`uq_issue_delegation_return_dispatcher`).
Each row names the dispatcher Agent and either:

- `managed_task` — the source run (`source_task_id`) identified by the
  server-stamped `X-Task-ID` of a `mat_` task token; or
- `external_session` — the registered external host session the dispatcher
  named explicitly.

A different dispatcher delegating on the same issue keeps its own row; it is
never addressed back to the first actor. The first accepted address for one key
is fixed: reassignment, rerun, or an ordinary mention by that actor cannot
rewrite it. Explicit takeover
(`POST /api/agents/{id}/session-bindings`) still changes the external binding,
and the accepted row follows the binding's frozen target.

`agent_task_delegation_return` links a dispatched run to every accepted
relationship it carries. An independent dispatch carries its own dispatcher's
relationship. When several dispatchers' requests coalesce into one pending run,
the run keeps every relationship, so a later result can notify each party
instead of silently only the first. A rerun preserves the source run's set, and
an executor reassignment preserves the issue's accepted set, then adds the
rerunning/reassigning agent's own relationship.

A backlog delegation records its address with no run yet. The relationship stays
unlinked until the first run starts on the issue; that run attaches every
unlinked relationship, so the later start never guesses whom to notify.

## Resolution and refusal

The gate reads only server-stamped authentication context (`X-Actor-Source`,
`X-Agent-ID`, `X-Task-ID`) plus an explicit `X-Return-Session` reference; it
never trusts client identity headers and never selects an Agent's latest or only
session. Every dispatch resolves the current actor: an issue-level row from an
earlier dispatcher is never evidence that this dispatch has an address.

| Dispatcher | Accepted evidence | Refusal |
| --- | --- | --- |
| managed Agent | running task for the same Agent and workspace, matching runtime, persisted provider session and work directory | `delegation_return_not_ready` (409) until the asynchronous pin lands; foreign, terminal, or cross-workspace runs are `delegation_return_invalid` |
| external Agent | `X-Return-Session` resolving to exactly one session registered to that Agent in the workspace (registered id or provider session id) | `delegation_return_missing` / `delegation_return_invalid` (400); no fallback to the human owner's profile |
| member | none required | — |

A managed run's source scope is its original issue, chat, or autopilot; the
delegated issue is not a substitute scope. A run with no business scope (for
example a quick create) is still an exact runtime/provider session/work
directory, and the task token's workspace binding proves tenancy.

## Entry points

- **Issue create**: the HTTP handler resolves the address before the write. The
  service inserts the issue, the accepted return, and an immediate assignee or
  squad-leader task in one transaction; the optional Composio overlay and the
  runtime wakeup happen only after commit. A failure at any of those writes
  rolls the whole create back. Backlog delegations record the return and start
  nothing.
- **Assignment / workflow handoff**: an explicit assignee change validates the
  address before the issue update commits and records it inside that update's
  transaction. A status-only workflow handoff is not an assignment and starts no
  run, so it is not gated. A backlog assignment records the address without a
  run.
- **Rerun**: preserves the source run's accepted set and adds the rerunning
  agent's address; it never changes who receives the result.
- **Managed mention**: a comment that resolves to at least one executable native
  run requires the same address before the comment is written. The return row is
  inserted inside the comment transaction, so a failed comment leaves no phantom
  accepted return. A mention that resolves only to blocked targets keeps the
  comment and reports the per-target blocked outcome. A comment folded into a
  pending run adds the new dispatcher's relationship to that run.
- **Batch update**: each item's prospective run and address are validated before
  its write. A refused item keeps its assignee, status, parent, and stage
  unchanged, is not broadcast, and is not counted as updated; other items still
  update and process child/stage events.

Members, issues with no executor, status-only writes, and discussions without an
executable mention are unchanged.

## CLI context

`--return-session` / `MULTICA_RETURN_SESSION` names the external session and is
sent as `X-Return-Session`. Outside a managed task the current Paseo session id
(`PASEO_AGENT_ID`) is the default. Inside a managed task the task token is
authoritative, an explicit session is refused, and an inherited provider session
is ignored.

Business commands that delegate (issue create/update/assign, comment add/update,
rerun) are marked as agent business commands. When `PASEO_AGENT_ID` is set and no
agent credential is available, they fail closed instead of silently
authenticating as the human profile that owns the machine — that human actor
would be exempt from the server gate. Explicit owner registration and credential
management commands are not marked and remain available to a human.

## Remaining work

Result submission that consumes `agent_task_delegation_return` and persists the
notification obligation atomically, delivery through the existing managed
queue/resume path or mailbox, visible failure after session deletion, explicit
takeover UI, and the isolated qualification described in the cutover runbook are
not part of this slice.
