# Delegation return gate

This slice implements the transactional delegation half of
[task-session-delivery.md](../design/task-session-delivery.md): an Agent that
hands work to another executable Agent or squad must name the address its result
returns to, and that address is accepted before any child work exists. Result
submission, notification delivery, takeover UI, and the maintenance-window
cutover remain later slices.

## Accepted returns

`issue_delegation_return` records one accepted relationship per
`(workspace, issue, dispatcher agent, assignee)` dispatch
(`uq_issue_delegation_return_dispatch`). The assignee is the exact assignment
slot that was handed the work (`agent`/`squad` and its id). Each row names the
dispatcher Agent and either:

- `managed_task` — the source run (`source_task_id`) identified by the
  server-stamped `X-Task-ID` of a `mat_` task token; or
- `external_session` — the registered external host session the dispatcher
  named explicitly.

A different dispatcher, and the same dispatcher handing work to a different
worker, keep their own rows. A row is never addressed back to an unrelated
dispatch. The first accepted address for one key is fixed: reassignment, rerun,
or an ordinary mention by that actor cannot rewrite it. Explicit takeover
(`POST /api/agents/{id}/session-bindings`) still changes the external binding,
and the accepted row follows the binding's frozen target.

`agent_task_delegation_return` links a dispatched run to every accepted
relationship it carries. An independent dispatch carries its assignment slot's
relationships. When several dispatchers' requests coalesce into one pending run,
the run keeps every relationship, so a later result can notify each party
instead of silently only the first. A rerun preserves the source run's set and
adds its target assignment's relationships. An executor reassignment preserves
the **replaced assignment's** relationships, then adds the reassigning
dispatcher's row for the new assignment; an unrelated worker's dispatch on the
same issue is not copied.

A backlog delegation records its row with no run yet. The relationship is keyed
to the assignment it was dispatched to, so the later run for that assignee
attaches it directly. It is never inferred from "every return on the issue" or
from "whatever row no run happens to carry": another worker's run cannot consume
it, and another dispatcher's relationship cannot be substituted for it.

The fold of a comment into a pending run and the return link that fold adds are
one transaction. If the link fails, the fold rolls back too, so a run never
carries a comment while silently missing one of its accepted dispatchers. The
accepted return row itself stays durable and a later fold attaches it.

## Resolution and refusal

The gate reads only server-stamped authentication context (`X-Actor-Source`,
`X-Agent-ID`, `X-Task-ID`) plus an explicit `X-Return-Session` reference; it
never trusts client identity headers and never selects an Agent's latest or only
session. Every dispatch resolves the current actor: an issue-level row from an
earlier dispatcher is never evidence that this dispatch has an address.

| Dispatcher | Accepted evidence | Refusal |
| --- | --- | --- |
| managed Agent | active (not completed/failed/cancelled) task for the same Agent and workspace, matching runtime, persisted provider session and work directory | `delegation_return_not_ready` (409) until the asynchronous pin lands; `delegation_return_source_closed` (409) once the run ended; foreign or cross-workspace runs are `delegation_return_invalid` (400) |
| external Agent | `X-Return-Session` resolving to exactly one session registered to that Agent in the workspace (registered id or provider session id) | `delegation_return_missing` / `delegation_return_invalid` (400); no fallback to the human owner's profile |
| member | none required | — |

The managed source is re-read under a row lock **inside the transaction that
persists the new delegation**, together with its agent's authorization and the
session/work-directory pin. A request that authenticated while the run was
active but waited on another writer until the run completed is refused, and
nothing is written. This is separate from a relationship the run already
accepted: that stored row remains a valid delivery target after the source
completes, because the result-handling turn resumes its recorded provider
session by id. It is only new delegations that require an active source.

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
  dispatcher's address. The address is recorded and linked only in the enqueue
  transaction, after triage and invoke eligibility have passed; a refused rerun
  leaves no accepted return.
- **Managed mention**: a comment that resolves to at least one executable native
  run requires the same address before the comment is written. The return rows
  are inserted inside the comment transaction, one per assignment slot, so a
  failed comment leaves no phantom accepted return and a multi-target comment
  does not attach one target's relationship to another target's run. A mention
  that resolves only to blocked targets keeps the comment and reports the
  per-target blocked outcome. A comment folded into a pending run adds the new
  dispatcher's relationship in the same transaction as the fold.
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

Fail-closed identity is the default rule of the business client, not a list of
annotated commands: when `PASEO_AGENT_ID` is set and the credential is not an
agent (`mea_`/`mat_`) token, client construction refuses before any request is
sent. That human profile would be exempt from the server gate. Explicit owner
registration, credential management, and agent management bootstrap flows use a
separate owner client; ordinary human commands outside a Paseo session are
unchanged. A newly added business command therefore inherits the refusal without
being added to a list.

## Remaining work

Result submission that consumes `agent_task_delegation_return` and persists the
notification obligation atomically, delivery through the existing managed
queue/resume path or mailbox, visible failure after session deletion, explicit
takeover UI, and the isolated qualification described in the cutover runbook are
not part of this slice. The local worktree Finalize/report ordering and
directory-retention race remain with the result-submission slice; the source-row
lock here does not claim to fix the whole resource lifecycle.
