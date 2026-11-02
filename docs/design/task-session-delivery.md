# Task-scoped sessions and durable result delivery

## Outcome and scope

A Leader delegates an issue from an existing session. Multica runs a managed Agent, persists its result, and notifies the Leader's original session for acceptance or rework. The same Agent identity may coordinate different issues from different sessions; one session may coordinate several issues.

This contract covers delegations issued through Multica. Sub-agents created inside an Agent's harness remain entirely owned by that harness. Managed execution continues to use the existing task runtime; it is not moved into the mailbox. Delivery success, result submission, and acceptance are separate facts.

## Ownership and addressing

- Multica owns Agent identities, issues, issue-specific session bindings, authorization, result records, and notification production.
- The host owns the existing session. A session address includes provider, host instance, and host session identity. An Agent identity is not a session address.
- Store session identities separately from `(workspace, agent, issue) -> session` bindings. Multiple issues may reference the same session. Enforce one binding per exact key, with no implicit default-session fallback for delegation.
- Creating a delegated issue and its authenticated dispatcher's return binding is one database transaction. Reject missing, foreign, or invalid return addresses before any managed task is queued. Task launch must not outrun the committed binding.
- Managed executors' sessions are managed by Multica. Leaders do not register harness-internal sessions or sub-agents.
- Reassignment of the executor does not change the recorded return recipient. Explicit takeover identifies the replacement recipient for future notifications; it does not rewrite earlier delivery envelopes.

## Delivery contract

Application producers resolve an authorized issue binding before enqueue. The delivery core receives a workspace, an opaque canonical target reference, an idempotency key, a transport kind, immutable payload, and optional business references. It must not resolve a recipient's current binding during claim, retry, or receipt lookup.

The canonical target represents the same physical session everywhere. Candidate selection, advisory locking, and final lease checks use that same identity. Messages to one session serialize; different sessions of one Agent can progress independently. This does not promise FIFO across retries.

An idempotency key identifies one notification, not an issue or a session. Retrying the same request returns the original delivery; reusing its key with conflicting immutable content fails. New events receive new keys. Changing a route must not turn a producer retry into a second notification.

The message fixes its target and delivery ID. The adapter determines which authorized Multica worker can reach that target. A worker/daemon ID is not a mandatory immutable message field. Worker reassignment preserves the target, delivery ID, and host receipt identity.

Known busy means no host-side acceptance: retain the message for retry with its original ID. Timeout or lost acknowledgement means uncertain outcome: query the original receipt, never blindly resend or assign a new ID. An absent receipt alone does not prove non-execution. Host acceptance ends delivery responsibility; it does not establish task completion or acceptance.

## Results, cancellation, and unavailable sessions

Persist results in the existing issue/comment records. Persist the obligation to notify atomically with result submission, so a crash between saving a result and notifying cannot lose the notification. Reuse the existing PostgreSQL transaction and delivery infrastructure; do not introduce another message broker or parallel result store.

A submitted result awaiting acceptance and an undeliverable notification can coexist. Show both on the issue. When the original session has been deleted, preserve the result and a visible delivery failure; do not silently redirect or rerun the managed Agent. An explicitly selected takeover session may receive a new notification of the existing result. Original uncertain deliveries remain intact and receipt-only.

Issue cancellation atomically withdraws notifications that have not been claimed. Cancellation and claim need a defined serialized boundary. An already accepted task requires execution cancellation through the managed runtime; withdrawing a notification cannot undo execution. Archiving or revoking a recipient must not create a window in which otherwise forbidden queued work is sent.

## Acceptance evidence

Use isolated databases and dedicated host sessions. Cover:

1. Two issues for one Agent route to different sessions; two issues sharing a session serialize. Include concurrent claim workers and final lease enforcement.
2. Delegated issue creation with its return binding is atomic; missing or unauthorized targets leave neither executable tasks nor a partial delegated issue.
3. Same notification retry retains one row, target, and ID; a second result/rework event creates its own notification; conflicting key reuse fails.
4. Managed result submission leaves a durable notification obligation. The original Leader session receives the exact notification and can accept or request rework.
5. Busy then idle delivers once; acceptance followed by lost acknowledgement recovers by original receipt; unknown and absent receipt never trigger a blind resend.
6. Session deletion leaves a visible notification failure and preserves the result. Explicit takeover does not rerun the executor or mutate the original envelope.
7. Cancellation races with claim at the defined boundary. Pending work is withdrawn; already accepted work is never reported as withdrawn.
8. Existing comment and generic wakeup producers obey the same target and authorization contract. No unscoped single-session lookup remains on the new production claim path.
9. Database-backed tests and real server/daemon/CLI/Paseo acceptance are separate gates. Record exact revisions and remaining gaps. Build success alone is insufficient.

## Development and release

Implement in isolated worktrees based on the deployed application revision `5f61732052390f17077868536e053cf0b080f9a2`, retaining its TGit integration and migrations through 610. The integration branch is `feat/task-session-delivery`; its worktree is `repos/multica-mailbox-redesign`. Managed task worktrees remain owned by their runtime. Existing WORK-49/50/51 and their worktrees retain their ownership. Inspect reusable CLI identity work without restarting those tasks or importing unrelated plugin changes.

Deliver three coordinated slices: session model and frozen-target delivery; transactional delegation/result notification and CLI; issue UI plus explicit takeover, migration rehearsal, and end-to-end qualification. Review the first tested production slice before expanding. The final version must satisfy all sections, not merely the first slice. Agent-built PRs stay Draft pending user verification; no merge or production deployment is authorized by the development task.

Use a maintenance-window cutover rather than mixed-version readers/writers. Rehearse with a production-data copy, inspect exact deployed versions, stop new dispatch, let existing managed runs reach a safe terminal state, then fence every message writer and claimant. Back up the database and deployed artifacts before migration. Unknown rows keep their IDs and receipts. Backfill a legacy target only with reliable evidence of its original destination; quarantine unresolved rows visibly instead of guessing from the current binding. Upgrade server, delivery daemon, and calling clients together, run one dedicated production acceptance task, then reopen dispatch. Do not remove historical data or old columns in this release.

Before new writes, an exercised rollback may restore the old application. After new writes, pause dispatch and preserve the new records; downgrading binaries or restoring an old database is not assumed safe. The cutover runbook must explain the actual supported recovery boundary and preserve new results.

## Non-goals

No general pub/sub redesign, CI producer API, broker replacement, daemon-per-message routing field, mandatory bounce/cache subsystem, new harness sub-agent lifecycle, or automatic rerouting to an arbitrary Agent session. Add no compatibility dual-write mechanism solely to support an unrequested rolling upgrade.
