# Delegation return gate

This slice implements the transactional delegation half of
[task-session-delivery.md](../design/task-session-delivery.md): an Agent that
hands work to another executable Agent or squad must name the address its result
returns to, and that address is accepted before any child work exists. Result
submission, notification delivery, takeover UI, and the maintenance-window
cutover remain later slices.

## Accepted return

`issue_delegation_return` holds at most one row per issue (enforced by
`uq_issue_delegation_return_issue`). It names the dispatcher Agent and either:

- `managed_task` — the source run (`source_task_id`) identified by the
  server-stamped `X-Task-ID` of a `mat_` task token; or
- `external_session` — the registered external host session the dispatcher
  named explicitly.

The first accepted delegation owns the result recipient. Reassignment, rerun,
an ordinary mention, and a comment folded into an existing run all reuse it;
they never rewrite it. Explicit takeover (`POST /api/agents/{id}/session-bindings`)
still changes the external binding for future notifications, and earlier
delivery envelopes keep the target they froze.

`agent_task_queue.delegation_return_id` records which accepted return a freshly
dispatched run belongs to. It is written with the run and never overwritten by
comment coalescing, so the result path can use per-run precision while the issue
row stays authoritative when the link is absent.

## Resolution and refusal

The gate reads only server-stamped authentication context (`X-Actor-Source`,
`X-Agent-ID`, `X-Task-ID`) plus an explicit `X-Return-Session` reference; it
never trusts client identity headers and never selects an Agent's latest or only
session.

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
  service inserts the issue, the accepted return, and an immediate assignee task
  in one transaction; the optional Composio overlay and the runtime wakeup
  happen only after commit. A failure at any of those writes rolls the whole
  create back. Backlog delegations record the return and start nothing.
- **Assignment / workflow handoff**: the address is validated before the issue
  update commits. The accepted return (existing or newly recorded) is committed
  before the run is inserted, and the run is linked to it.
- **Rerun**: reuses the accepted return or records the rerunning Agent's
  address; it never changes who receives the result.
- **Managed mention**: a comment that resolves to at least one native (managed)
  target requires the same address before the comment is written, so a refused
  delegation leaves no comment and no task. A comment folded into a pending run
  keeps that run's original return link.

Members, issues with no executor, status-only writes, and discussions without an
executable mention are unchanged.

## CLI context

`--return-session` / `MULTICA_RETURN_SESSION` names the external session and is
sent as `X-Return-Session`. Outside a managed task the current Paseo session id
(`PASEO_AGENT_ID`) is the default. Inside a managed task the task token is
authoritative, an explicit session is refused, and an inherited provider session
is ignored.

## Remaining work

Result submission that persists the notification obligation atomically, delivery
through the existing managed queue/resume path or mailbox, visible failure after
session deletion, explicit takeover UI, and the isolated qualification described
in the cutover runbook are not part of this slice.
