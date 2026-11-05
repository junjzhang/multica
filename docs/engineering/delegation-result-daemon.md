# Managed delegation result execution and resource retention

A result-handling claim is a new task in the original conversation. Its
`delegation_result` identifies the saved delegated result and the accepted
runtime, provider, session, and working directory. The daemon verifies those
fields against the claim and passes `RequireResume` to the provider. It does
not look up a later source task or choose the latest session.

The new turn carries the actual callers' accepted upstream returns. The frozen
address's `source_task_id` identifies the original conversation, not the caller
of every later delegation to that conversation. Comment, assignment and run
return joins snapshot `caller_task_ids` when work is accepted. Coalescing unions
these sets; retries, replay and reassignment preserve them. Editing a comment
replaces its current intent without rewriting past runs' copied provenance.

Creating the turn, recording its incoming notification and copying those
callers' upstream relationships commit together. Callback authority comes from
the callers' consistent originator, independently of the original address's
scope and resume identity. A new incompatible source scope or conflicting
pending caller authority is refused before accepting the delegation.
A saved continuation outcome therefore returns to the actual dispatchers even
after several downstream reviews, without changing their frozen destinations.

A null caller snapshot means historical provenance is unknown; an empty set
means a known external caller. Unknown managed provenance still permits delivery
to the frozen session, but grants no inferred human authority and does not guess
upstream returns. The verified result context sets `upstream_returns_unknown`;
this diagnostic follows durable incoming edges across further result turns.
`accepted_returns` continues to list actual outgoing obligations, so its empty
array does not resolve an explicitly unknown ancestor. No context JSON flag is
trusted as provenance or authorization evidence. Already materialized legacy
turns with unproven nonempty authority are refused; migration does not silently
retain that authority or rewrite the turn. Before upgrading, inspect and drain
or explicitly resolve queued pre-upgrade callbacks. There is no automatic
backfill of old caller authority.
Each outcome has its own notification identity; it is not business acceptance.
Reconciliation repairs relationships linked after the source turn finished,
including links arriving through retries. A return already traversed by the
same result-propagation chain is not copied again, so mutual delegation cannot
produce an unbounded automatic response loop. Explicit post-result takeover
notifications remain one-off and do not inherit the chosen recipient's work.

A strict turn must reuse the accepted directory under its env-root lock. A
missing directory, changed ownership, unsupported source environment, or
provider refusal before input produces `delegation_result_unavailable`.
Once provider input may have happened, an unsuccessful or ambiguous outcome
produces `delegation_result_input_unknown`. A nominal success without a reported
session address also remains unknown; prior hints are never filled in as receipt
evidence. Neither outcome permits a fresh
prompt retry. A post-prompt resume rejection cannot override this rule.
Busy directories and provider stores keep the same task waiting with its
preparation lease; cancellation ends that wait. Only the combination
`ResumeRejectedTransient && StrictResumeUnmet` proves a pre-input busy refusal
and permits waiting in the same task before retrying the same strict session.
`StrictResumeUnmet` alone does not prove that no input occurred: it can describe
an error after input and must remain unknown, without retry or resource release.
An ambiguous or post-input failure never takes the busy-retry path. Starting a task or observing
a session status event is not evidence that the result input was accepted.

The prompt references the saved result and asks the original conversation to
assess it. It does not repeat the delegated executor's instructions. Task
credentials, MCP credentials, and current authorized configuration belong to
the new task. A retained session is not permission to reuse the source task's
closed credentials.

Claims and issue run reads include `accepted_returns`, the complete accepted
automatic-return relationships at that read. Each entry identifies the frozen
return and recipient without exposing provider-store paths or credentials.
An explicit empty array means the read found no relationship; an absent field
means unknown, including an older server. A failed read fails the claim or API
request instead of presenting an empty set. The daemon adds this snapshot to
each turn, including result-handling turns. Before an explicit final handoff,
refresh the run because late dispatch links may have added relationships.
`result_notifications` describes outcome delivery and cannot establish that
an active run has no accepted return.

A v1 claim supplies a new `session_pin_generation`. Provider status callbacks
carry that generation; an asynchronous callback never reads a later attempt's
value. Before an ordinary fresh retry or worktree repair turn, the daemon uses
the existing `/session` endpoint to invalidate the previous generation with an
idempotent compare-and-swap. A missing or unknown acknowledgement prevents the
next provider invocation. A late previous Pin cannot confirm the new attempt.
The server gates new managed delegation on the current attempt's confirmed Pin,
not on a retry's copied session hint. Existing accepted return snapshots remain
unchanged.

For a source with no issue, chat, or autopilot, `scope_task_id` identifies the
original task namespace. A handling task R uses fresh R credentials while its
provider state stays in source S's environment. Further result turns preserve
S rather than creating a store keyed by R. Task-specific worktree branches persist
the known workspace, agent and original-task owner while keeping their existing
name and `tracks_state=false`. Preparation saves that identity before provider
execution and the v1 session pin; strict reuse still rejects missing or conflicting
owner metadata. A deleted business scope cannot be
reinterpreted as this task-only scope. Its runtime brief describes result
processing without an issue-comment or quick-create workflow. Source quick-create
execution fields are cleared, while the new process receives its current task
identity and credentials.

## Retained execution environments

`LocalWorktree.Finalize` commits the work and records branch ownership without
removing the checkout. A persisted worktree descriptor records the preparation
state and successful finalization checkpoint. Exact reuse checks the original
repository, checkout path, branch, conversation owner, and directory identity.
It works when the current project resource has changed; the accepted directory
remains the target. User-owned in-place directories without this managed proof
are refused by strict reuse rather than adopted.

For Hermes, strict reuse verifies that `state.db` and `memories` still refer to
the proven original stores. Skills and current configuration can be refreshed,
including removal of the last skill, without deleting that session home.
A different profile store is a visible refusal. Ordinary reuse retains its
existing refresh and fallback behavior.

The task handler performs these operations in order:

1. Persist the provider's work and report its actual outcome.
2. Obtain an authoritative terminal acknowledgement. An unknown report retains
   the directory; a locally queued report alone is insufficient.
3. Reacquire the env-root lock and verify current resource ownership.
4. Request `/api/daemon/resources/release` for the actual deletion root.
5. Remove only the finalized, unchanged checkout after permission is granted.
6. Report `resources-released` to refine `durable_work_dir` after real removal.

A denied release is normal retention, not a failed provider result. A removal
failure also retains the checkout. The result and committed branch remain
available. A failed metadata acknowledgement does not rewrite the saved result
or claim that an unremoved directory was deleted.

## Garbage collection

GC uses the same release endpoint and the existing TTL policies. It locks the
current env root, rereads owner and completion metadata, and discards a stale
scan decision when a later task has taken over. New environments also record
the current runtime and resource group locally; this is GC ownership evidence,
not a replacement for the server's immutable accepted return address.

A checkout already removed after finalization does not keep its remaining env
root forever. GC first requires the current owner's persisted deletion receipt
and terminal execution acknowledgement. Its original runtime, provider, agent,
session and workdir must still match the saved resource identity. The existing
finalization checkpoint must also prove the durable repository is accessible
and still contains the saved work under the recorded branch ownership. Missing
receipts, inaccessible storage, changed identities or unresolved dependencies
retain the env root. A missing checkout by itself never permits deletion.
The saved local conversation owner remains authoritative if quick-create later
links the original task to an issue; the current task's scope projection is not
used to reconstruct that original namespace.

Older env roots can prove ownership through `.task_owner` and `.gc_meta.json`.
The release request supplies that workspace/task identity and the actual root;
it does not invent an old session. Older provider stores retain their existing
profile/agent/conversation layout as ownership evidence. The server checks all
matching dependencies on the authenticated daemon, including frozen historical
addresses, planned retries, pending or unknown notifications, and active result
turns. Missing, malformed, or unavailable release decisions preserve resources.
For directory release the response includes every protected address in the
authorized daemon/workspace scope, without truncation. While holding the env
lock, the daemon checks canonical paths, containment, and `os.SameFile`; a
historical symlink or filesystem alias cannot evade protection merely because
its stored spelling differs. Unresolvable addresses retain the resource.

Execution and GC use the same OS locks. Provider-store lock files live outside
the deletion root. Codex and Hermes session stores take exclusive writer locks;
Hermes memory users share a use lock so independent conversations remain
concurrent, while GC requires exclusive access. The daemon takes these locks
before provider execution and session pinning. After authorization, env-root
GC detaches the directory before recursive removal so a new claimant cannot
acquire a newly created lock inode inside the tree being deleted.

The explicit regenerable managed-cache cleanup policy, such as `.sandbox-bin`,
may run while an idle conversation is retained. It still requires the env-root
OS lock. This exception does not cover user artifact patterns, workdirs,
provider transcripts, or result evidence. Empty task-root index entries are
metadata only; pruning them does not delete an execution directory.

## Mailbox consumption and wakeups

Every delegated outcome has one `agent_mailbox_message`, including managed
results. Its stable notification key is `result:<outcome-task>:return:<return>`.
The delegation link records the result, frozen return, mailbox ID and optional
managed wakeup task. It does not own a second processing state. Managed mailbox
rows have no external target session and cannot enter the external delivery
worker; their wakeup status is the associated task's status.

A live managed task reads its addressed results with:

```sh
multica agent mailbox list "<recipient-agent-id>" --kind delegation_result --output json
multica agent mailbox ack "<recipient-agent-id>" "$MESSAGE_ID" --state handled
```

Follow `next_cursor` using `--after` until the page has no cursor. The payload
contains saved result and return references; reading never acknowledges them.
`read` and `handled` are explicit, monotonic receipts. ACK is not issue
acceptance, a new business result, or authority to merge a pull request.

Managed reads and ACKs require the receiving task credential, workspace, pinned
runtime/session/workdir, business scope, and actual callers' originator
authority. Another scope or authority of the same agent cannot consume the
result. Human owners may inspect managed mailbox records, but cannot consume
on behalf of a task. The first managed consumer is recorded as
`handled_by_task_id` on the mailbox.

ACK locks the consumer and wakeup tasks in ID order, then changes the mailbox
receipt and cancels only unstarted wakeups in the same transaction. Queue claim
also excludes handled messages. Start and ACK serialize on the task row: a
started task continues; a cancellation that wins makes start refuse. The
consumer inherits the cancelled wakeup's real upstream obligations. Later
reconciliation follows that consumer for late caller links, and provenance
checks follow all consumed incoming results so unknown ancestry stays unknown.
No ACK itself produces a completed task or delegated outcome.

A daemon whose exact claim was cancelled during preparation receives
`cancelled_claim: true` from `/start`. After its runner exits, it sends a
cancel-ack fenced by the original runtime and dispatch timestamp. A stale
claim cannot prove another owner's exit. Older servers do not emit that marker;
new daemons keep their old stale-claim behavior against them. Lost exit receipts
retain resources until the receipt is recovered; ACK never fabricates exit.

A handled result no longer pins resources after its executions have exited.
Active tasks and missing resource-control exit receipts still retain them.
Completed but unacknowledged result tasks remain pending consumption; business
acceptance and execution exit remain separate release requirements.

External results use the same processing receipt. A handled pending result is
withdrawn before host acceptance. Leased or unknown transport states retain
their exact lease and receipt and are verified using the existing delivery ID;
withdrawal prevents a known no-effect response from returning to the send queue.
The existing external credential remains agent/workspace scoped. This change
does not create per-session external credentials or claim isolation between
sessions sharing that credential.

## Mailbox migration boundary

Migrations 646–648 backfill mailbox records for historical managed wakeups and
add consumer provenance and the result-to-message index. Existing external
message IDs, destinations, receipts and unknown states are preserved. Backfill
does not infer `handled` from a completed run. Inspect historical notifications
and arrange explicit consumption from an authorized pinned task when required.

Quiesce result writers and drain in-flight daemon preparation before applying
these migrations. Upgrade all API/result-reconciliation writers together, and
upgrade daemons before enabling managed consumption: an old daemon cannot
report the new cancelled-before-start receipt. This is a coordinated upgrade,
not a mixed old/new writer rollout. The database constraint rejects old writers
that create a managed notification without its mailbox record. Rollback refuses
to discard existing managed mailbox/consumer facts; recover forward rather than
running old consumers against acknowledged notifications.

## User directory ownership and upgrades

A project `local_directory` cannot point into daemon-owned workspaces or a
proven task subtree owned by another profile. Prepare, local-directory Reuse,
and execution admission check the canonical directory before writing task
configuration or starting the provider. A normal user directory remains usable
when it contains Multica context sidecars; those sidecars are not task-root
ownership. A nonempty task owner also prevents user-directory adoption when
its legacy marker lacks workspace or completion metadata; missing proof never
converts a daemon-owned directory into a user-owned one. Managed strict result
reuse continues through its own root lock and provenance checks.

This boundary does not repair historical registrations. Before upgrading a
daemon that may have used this configuration, stop its execution and GC, then
inventory every attached workspace's local-directory resources and persisted
task working directories. Resolve aliases on that host and move any borrowed
managed subtree into a user-owned directory, updating its bindings before
restarting. An incomplete inventory or remaining overlapping binding blocks
that transition. The parent managed root can otherwise still be reclaimed even
if the borrower will no longer execute: old GC metadata stores only a
`local_directory` flag, and sidecar manifests do not enumerate all bindings.
Workspace-scoped release authorization is deliberately not widened to obtain
another workspace's private addresses.
