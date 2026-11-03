# Delegation return gate

This slice implements the transactional delegation half of
[task-session-delivery.md](../design/task-session-delivery.md): an Agent that
hands work to another executable Agent or squad must name the address its result
returns to, and that address is accepted before any child work exists. Result
submission, notification delivery, takeover UI, and the maintenance-window
cutover remain later slices.

## Canonical binding, intents, and runs

The accepted address is one row per `(workspace, issue, dispatcher agent)` in
`issue_delegation_return` (`uq_issue_delegation_return_dispatch`). It does not
include the assignee: the address belongs to the dispatcher, and the worker a
dispatcher chooses does not change where that dispatcher's results go. Each row
names either:

- `managed_task` — the source run (`source_task_id`) identified by the
  server-stamped `X-Task-ID` of a `mat_` task token, plus its frozen
  `runtime_id`, `provider`, `session_id` and `work_dir`; or
- `external_session` — the registered external host session the dispatcher
  named explicitly.

Which dispatch an accepted relationship belongs to is recorded separately:

- `comment_delegation_return(comment_id, delegation_return_id)` is the comment
  intent. A comment is a stable business id, so new/fold/reconcile/retrigger
  can all read the same relation instead of an in-memory set.
- `issue_assignment_delegation(issue_id, assignee_type, assignee_id, task_id)`
  plus `issue_assignment_delegation_return` is the current assignment intent.
  `task_id` is empty for a backlog assignment and points at the run once it
  starts.
- `agent_task_delegation_return(task_id, delegation_return_id)` is the set of
  accepted relationships a run actually carries.

A run links only the intents it covers:

- a new comment run links that comment's relationships;
- a fold links every folded comment's relationships;
- completion replay and edit retrigger link the replayed comment's
  relationships;
- a rerun links the exact source run's set plus the rerunning actor's
  relationship;
- a reassignment links the replaced assignment's intent and corresponding run,
  plus the reassigning actor's relationship.

Nothing collects "all returns for this issue", "all returns for this worker", or
"the worker's latest run". That is what keeps two independent requests to the
same worker from inheriting each other's recipient.

## Comment recovery closure

An agent-authored comment with at least one executable target records one
`comment_dispatch_obligation` per target in the comment's own transaction. The
row names the exact executing agent, optional squad, trigger source and accepted
return; replay never re-derives them from mutable comment text or the issue's
current assignee. The same transaction then tries to cover the target through
the one normal rule:

1. fold into a pre-claim run in the same thread (`MergeCommentIntoPendingTask`,
   with the folded comment's return links);
2. register a planned input on a claim-receipt run only when the comment
   predates that run (the lost enqueue race);
3. otherwise create the run and its return links in the comment transaction.

Every attempt is a savepoint. A target that cannot be covered yet leaves its
obligation pending; the comment and accepted return still commit. There is no
dependency on a future completion event for a pending obligation.

The pending obligation is consumed by a real periodic consumer,
`runCommentDispatchObligationSweeper` (wired in `cmd/server`), which locks one
live comment and then its obligation, re-checks current invocation permission
and runtime readiness, and settles only with durable run coverage. Recovery
locks issue, comment, obligation, then task. Edit and delete also lock issue and
comment before touching obligations or tasks. A legacy edit cancels its old
tasks inside the same locked transaction as the new body;
no consumer can insert old work between cancellation and obligation removal.
A crash after the comment commit therefore leaves a row the restarted server can advance
without another run completion and without a person editing the comment.
Transient failures are retried with bounded backoff. Authorization uses the
stored comment's author and `ResolveOriginatorFromTriggerComment`, then the
same `canInvokeAgent` gate as normal dispatch. A missing or archived agent author
fails closed. Squad dispatch checks the accepted leader's permission and requires
that leader still belongs to the accepted squad role; it never silently selects
a replacement leader. Revocation records a failed obligation with
`invocation_not_allowed`; an unavailable author, target, squad, comment or issue
also leaves a diagnostic failure when its obligation still exists. This is a
small durable work row consumed by an existing sweeper pattern, not a second
broker or generic execution queue.

A comment newer than an active run stays out of that run's planned set, since
completion reconciliation already finds it by timestamp. It may immediately
create a queued follow-up alongside a running task. If a claimed task still
occupies the pending slot, the obligation stays pending until it can fold or
start a follow-up. A comment predating an active run must instead be recorded
in that run's planned inputs so completion reconciliation can find it.

Completion replay, edit retrigger and their recovery fallback use the same
locked transaction for accepted obligations. Run creation or folding, return
links and settlement commit together. A settlement failure rolls the attempted
coverage back and preserves a pending retry, including when a previously
planned input needs a follow-up. There is no best-effort settlement after an
independently committed enqueue.

`settled_task_id` records the covering run for this obligation generation. Its
delivered-comment receipt proves that the input reached that run, not that its
business work completed. A covering run that received the input remains
responsible for it; replay restores missing accepted return links without
creating another execution. A completed run that only planned the input still
needs replay. A failed run's received inputs stay with the task failure/retry
policy, not a second independent comment execution chain; its receipt does not
claim success. Editing the body removes the old obligation and records a new
one: an old run's receipt for the same comment id cannot satisfy the new request.

Editing or deleting one input can cancel a whole comment batch. That cancellation
reopens the surviving comments' accepted obligations in the same transaction,
retaining their exact target, return relationship and original covering task.
The cancelled task's delivered-input receipt proves admission, not completed
work: recovery rechecks current authorization and covers those surviving intents
again. A completed covering run remains idempotent, and an explicit user
cancellation does not reopen obligations. The changed comment's old obligation
is replaced or removed by its own mutation; it is not a surviving intent.
Deletion, batch cancellation and survivor reopening commit together. If deletion
fails, the original comments, run and accepted obligations remain unchanged;
post-commit cancellation events and survivor replay only run after success.

## Resolution and refusal

The gate reads only server-stamped authentication context (`X-Actor-Source`,
`X-Agent-ID`, `X-Task-ID`) plus an explicit `X-Return-Session` reference; it
never trusts client identity headers and never selects an Agent's latest or only
session. Every dispatch resolves the current actor: an issue-level row from an
earlier dispatcher is never evidence that this dispatch has an address.

| Dispatcher | Accepted evidence | Refusal |
| --- | --- | --- |
| managed Agent | active (not completed/failed/cancelled) task for the same Agent and workspace, matching runtime, v1 resource-control claim and a provider session/work-directory pin confirmed for the current execution generation | `delegation_return_not_ready` (409) until the asynchronous pin lands; `delegation_return_source_closed` (409) once the run ended; foreign or cross-workspace runs are `delegation_return_invalid` (400) |
| external Agent | `X-Return-Session` resolving to exactly one session registered to that Agent in the workspace (registered id or provider session id) | `delegation_return_missing` / `delegation_return_invalid` (400); no fallback to the human owner's profile |
| member | none required | — |

The managed runtime, agent and source task are locked in that order **inside
the transaction that persists the new delegation**. The source runtime supplies
the provider; the locked source task supplies its runtime, session and work
directory. These four address values are copied to the accepted return row. A request that authenticated while the run was
active but waited on another writer until the run completed is refused, and
nothing is written. This is separate from a relationship the run already
accepted: that stored row remains a valid delivery target after the source
completes. `source_task_id` preserves the original issue/chat/autopilot scope as
provenance; it is not an address lookup. Ordinary task pin, completion and
fallback may change or clear that task's resume fields without changing the
accepted snapshot. It is only new delegations that require an active source. The
sweeper's replay of an accepted comment executes that stored intent and does not
accept a new delegation from a closed source; it still applies the current
revocation/runtime checks.

A managed run's source scope is its original issue, chat, or autopilot; the
delegated issue is not a substitute scope. A run with no business scope (for
example a quick create) is still an exact runtime/provider session/work
directory, and the task token's workspace binding proves tenancy. A continuation
turn may reuse the binding only when all four frozen address values match,
even if its task ID differs. The original `source_task_id` stays unchanged.
Reusing the same task ID does not bypass this comparison: a re-pin, different
runtime/provider or changed directory is a conflict requiring explicit takeover.
A missing snapshot fails closed; mutable task history is never a substitute.

Result delivery must use the accepted snapshot and strict session resume. It
must not reconstruct the destination from the source task's current fields or
an agent's latest run. If the frozen session, runtime or directory is unusable,
keep the result and surface delivery failure instead of choosing another
session. This strict return behavior is separate from ordinary task fallback,
which remains available for ordinary tasks; typed result turns require
`RequireResume` and cannot enter fresh fallback or ordinary rerun.

The snapshot columns belong to unreleased migration 618. Qualification requires
a fresh database with the final SQL and a replay from the 610 baseline before
cutover. A local database that already recorded the older 618 does not acquire
the snapshot columns by rerunning migrations. Do not backfill a claimed
original address from the source task's mutable current state.


## Result delivery and resource retention

`agent_task_queue.result` and `error` remain the outcome authority. Each actual
run/return pair in `agent_task_delegation_return` owns one notification pointer
or a visible refusal. The scheduler recovers from terminal tasks and accepted
links, including links committed after completion. Notification creation and
its pointer commit together. Repeated or concurrent consumers cannot create
another delivery or handling turn for that pair.

Completed tasks are eligible. Failed tasks become eligible only after the
existing retry policy durably decides that no automatic retry remains. A
conflicting pending slot leaves that decision unresolved; absence of a child
alone is not finality. Both retry creation paths copy the complete accepted
return set. Late links follow the exact `retry_of_task_id` chain. Cancelled
executions are control facts, not a new result-notification outcome. Runs with
accepted returns do not also use delegated-failure compensation.

External recipients use the existing generic delivery with the accepted
session id. Managed recipients use an ordinary queue row with typed result
input, the original scope, and the frozen runtime/provider/session/workdir.
Claim verifies the reverse durable notification relation before rendering the
input. A marker in context alone grants nothing. Unsupported executors are
visibly refused, including hosted paths that cannot prove exact resume. The
handling turn does not inherit the executor's outgoing returns, and ordinary
comments cannot merge into it. Real new delegations it accepts still create
their own obligations.

For an original task-only scope, the accepted return stores `scope_task_id`.
A typed handler that delegates more work proves that scope through its current
durable notification before the new return copies it. The new dependency then
owns that scope fact; removing the older notification does not change its
namespace. Quick-create completion may link the original task to a newly
created issue for navigation, but that later link cannot replace the frozen
task scope. Source and scope task records remain protected while accepted
business work or execution/notification dependencies are unresolved.

A typed terminal success must report the verified session and work directory.
Missing or contradictory addresses produce an input-unknown failure, retain
the original tuple and notification id, and never resend the input. Failure and
cancellation reports cannot retire or overwrite the frozen tuple. Resource
control callbacks report location refinement separately from outcome reporting.

Ordinary pending assignment/comment inputs keep their own unique issue/agent/
thread slot. Typed result turns are excluded from that index; their locked
run/return obligation and unique notification pointer provide idempotency.
Claim-time in-flight serialization still includes both kinds. Migrations build
the replacement index concurrently before retiring the shared index. Rollback
requires draining colliding pending turns; it never discards either input.
Transient notification failures retain the same obligation and record its last
attempt time, so a blocked prefix cannot starve later eligible recipients.

The daemon advertises `delegation_results_v1`. Claim commits
`resource_control_required`, a fresh `session_pin_generation`, and the task
token in one transaction. Old queued work acquires that agreement when claimed
by a v1 daemon; a task that already requires it cannot be reclaimed by an older
daemon. Inherited session/workdir values are resume hints until a matching
current-generation pin confirms both. Before ordinary fresh fallback, the
`/session` endpoint atomically changes the expected generation and clears its
confirmation. A repeated transition to the same new generation acknowledges it
without clearing a newer pin. Late old-generation pins cannot alter the task
or previously accepted snapshots.

Finalization commits work, reports the terminal outcome, and only then asks
`POST /api/daemon/resources/release` while holding the local resource lock.
`terminal_reported_at` records an actual authorized completion/failure/cancel
acknowledgement; a control-plane terminal transition cannot manufacture it.
Task completion alone is not business acceptance. Frozen directories and
provider stores remain protected until the delegated business is canonically
done/closed and every pending input, active execution, and pending/unknown
notification dependency has closed. A handling turn cancelled after it started
remains input-unknown even after its executor acknowledges exit. Its durable
notification pointer is retained and it is not automatically resent.

Resource queries cover the actual owned directory tree or provider-store
scope, including other sessions and runtimes on the same daemon. The returned
protection list is complete within the authenticated request workspace; it
never expands a workspace-bound daemon credential into another workspace.
Provider-store permission is decided by the server's complete provider/agent/
scope dependency check, not by a daemon path comparison. Supported stores are
keyed by profile and globally unique agent/scope identities. If a frozen return
loses its source/runtime lookup, its original scope is unknown: stores for that
provider and actor remain protected, with the frozen address retained in the
response. A current actor binding cannot reconstruct the original scope.

Legacy provider-store requests can omit task/runtime/workspace identity, but
must identify the actual daemon when using a user credential. The server derives
the agent's real workspace, checks membership, verifies registered daemon
ownership and current or historical hosting, and rejects conflicting identities.
A daemon token's workspace and daemon binding cannot be overridden by the body.
Missing ownership proof never grants release.

Local execution must reject using a daemon-owned task subtree as a user-owned
in-place directory, including physical aliases. Existing such bindings require
a maintenance inventory and relocation before enabling deletion; blocking new
bindings alone does not establish that legacy directories are safe. Workdir
responses include all protected addresses in the authorized daemon/workspace,
not only textual path matches. The daemon must also veto physical aliases and
overlapping directories under its resource lock; an incomplete or unverifiable
answer grants no deletion. Historical ownership metadata can authorize the
legacy TTL path when there are no new dependencies. It cannot reconstruct a
frozen return address or substitute for a v1 terminal acknowledgement.

After confirmed removal, `POST /api/daemon/tasks/{id}/resources-released`
idempotently refines only the top-level `durable_work_dir`. It neither rewrites
`result`/`error` nor creates another notification. A typed turn may prove its
original source through the durable reverse relation, but the receipt updates
the turn whose directory was actually removed. Removing historical directory
A never redirects a task that now points at B.

Issue run reads include only materialized/refused notifications or eligible final
outcomes. A comment-change cancelled run and an automatic-retry intermediate
attempt have no pending notification of their own; their replacement/final leaf
owns its actual result notification. This projection does not change resource
dependency accounting.

Issue run reads expose `result_notifications`. An explicit empty array means
that the server read the obligations and found none; an omitted field means
unhydrated or unsupported. Queued, dispatched, and running handling tasks are
not delivery receipts. External `delivered` means the host accepted the input;
managed `received` requires a completed handling turn. Unknown values and
malformed projections must remain unknown.

## Entry points

- **Issue create**: the HTTP handler resolves the address before the write. The
  service inserts the issue, the accepted return, and an immediate assignee or
  squad-leader task in one transaction; the optional Composio overlay and the
  runtime wakeup happen only after commit. A failure at any of those writes
  rolls the whole create back. Backlog delegations record the return and start
  nothing.
- **Assignment / workflow handoff**: an explicit assignee change validates the
  address before the issue update commits and records it inside that update's
  transaction. The assignment intent and the run's links commit with the issue
  write. A status-only workflow handoff is not an assignment and starts no run,
  so it is not gated.
- **Assignment conflict**: the fallback read matches
  `idx_one_pending_task_per_issue_agent_thread` exactly, including the
  channel-media deferred exception. A conflict is re-observed in a bounded loop:
  a still-pending occupant is linked, a run that started or disappeared lets the
  retry insert a covering run, and an occupant that cannot be resolved rolls the
  whole issue write back. `inserted=false` with no covering run is never
  committed.
- **Rerun**: with a `task_id`, the named run is used exactly. Without one, the
  rerun follows the current assignment intent's `task_id` (the run that
  assignment actually started), not the worker's latest run. With no
  determinable delegated assignment source the request is refused with a 400
  requiring `task_id`; the legacy latest-run fallback is kept only for an issue
  that has never accepted a delegation return and whose latest run carries none.
  The source run's set and the rerunning actor's address are recorded and linked
  only in the enqueue transaction, after triage and invoke eligibility pass.
- **Managed mention / comment**: the gate runs before the comment is written.
  The comment, its accepted return, and its durable dispatch obligations commit
  together. A mention that resolves only to blocked targets keeps the comment
  and reports the per-target blocked outcome, with no obligation.
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
managed takeover UI, and the isolated qualification described in the cutover
runbook are not part of this slice. The local worktree Finalize/report ordering
and directory-retention race remain with the result-submission slice; the
source-row lock here does not claim to fix the whole resource lifecycle.
