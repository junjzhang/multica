# Agent delivery middleware: first-slice contract

This document fixes the smallest production contract for the delivery core
extracted from the external-agent mailbox. It describes the domain the core
owns, how it maps onto the existing `agent_mailbox_message` table, and the
business writes that are now atomic with enqueue, and the first consume loop
with one host adapter. It is not the full event and wakeup middleware design;
event sources and generic scheduling stay outside this slice.

Background: [external-agent-mailbox.md](external-agent-mailbox.md).

## Scope

Implemented here:

- `server/internal/delivery`: transport-neutral request/state types and a
  PostgreSQL store.
- Comment submission persists every valid explicit external handoff in the
  same transaction as the comment.
- Comment edits that change content resolve mentions before the write and
  persist every valid external handoff in the edit transaction.
- One storage for old and new deliveries: `agent_mailbox_message`.
- Tests for core enqueue without issue data, idempotency, conflict, atomic
  rollback, multi-recipient failure, legacy UUID recovery and legacy-reader
  filtering.

The follow-up consume slice adds:

- `delivery.Queue` with the claim/lease/outcome operations the mailbox handler
  and daemon loop previously open-coded.
- A generic daemon worker and host contract (`DeliverIfIdle`, `LookupReceipt`)
  for deliveries with no issue or comment.
- A dedicated `unknown` delivery state and verify-only reclaims.
- An optional retry class so a busy session is not treated as a failure.

Deliberately unchanged:

- The frozen mailbox API DTO and its rendered comment notification.
- The external-agent identity/permission model and session/worktree ownership.
- `IssueWakeupService`, autopilot scheduling and CI watchers.
- Message batching and priority scheduling.

## Domain

```text
Enqueue(EnqueueRequest) -> Delivery
GetDelivery(scope, delivery_id) -> Delivery
```

An `EnqueueRequest` contains only stable identifiers:

| Field | Meaning |
| --- | --- |
| `Scope.WorkspaceID` | Isolation boundary for every lookup |
| `Source.Type`, `Source.Key` | What produced the request, and its idempotency key |
| `Source.ActorType/ID/SessionID` | Optional sender identity when the source is a session |
| `Recipient.Type/ID` | One authorized target; no broadcast in this version |
| `Kind` | Notification kind interpreted by the business adapter |
| `Payload` | Immutable JSON notification data, bounded and required |
| `Refs.IssueID`, `Refs.CommentID` | Optional stable references owned by the source; `CommentID` is comment-source-only |

The package does not import issue, comment, agent task or agent types. A
request with no refs is valid: a CI terminal event or background wait can use
the same durable path as an issue comment.

`Refs.CommentID` mirrors the legacy comment projection and shares its
`(recipient_agent_id, comment_id)` unique key, so `Validate` accepts it only
when `Source.Type == "comment"`. A generic producer keeps a comment reference
inside `Payload`; letting a generic row occupy that key would make a later
comment delivery look like a replay of an unrelated request.

`Enqueue` is idempotent on
`(workspace, recipient type, recipient id, source type, source key)`:

- same key and same immutable content returns the original delivery ID;
- same key and different content returns `ErrConflict` and stores nothing;
- the recipient is part of the key, so one recipient's retry can never consume
  another recipient's request.

Content comparison covers `Kind`, `Refs`, the source actor/session and the
canonical JSON payload. Key order and number spelling do not matter.

`GetDelivery` returns the stored request plus `DeliveryStatus`,
`ProcessingStatus`, attempts and host receipts. Delivery or read state never
means the requested work was accepted or completed.

## Storage

`agent_mailbox_message` remains the single authoritative table. Migration 604
adds `source_type`, `source_key`, `kind`, `recipient_type` and `payload`, and
makes the comment projection (`issue_id`, `comment_id`, `sender_type`,
`sender_id`) optional. Migration 605 adds the unique idempotency index
concurrently.

The existing columns are a projection, not a second state machine:

- `workspace_id`, `recipient_agent_id`, `source_session_id` are written from the
  request scope/recipient/source;
- `issue_id`, `comment_id` and `sender_*` are written when refs/actor are
  present, so the mailbox API, daemon claim and issue detail keep their current
  behavior;
- delivery status, lease, attempts, delivery/processing receipts and
  `created_at` stay in the same row and under the same lifecycle.

Migration 604 backfills legacy rows with
`source_type='comment'`, `source_key=comment_id`, `kind='comment_mention'` and
the stable comment reference payload. Existing mailbox UUIDs, leases and
receipts are not rewritten. A replay that reaches a row written before the core
(lookup by the legacy `recipient_agent_id, comment_id` key) returns that
original UUID instead of inserting a second request.

`CreateDelivery` uses `ON CONFLICT DO NOTHING` without a conflict target on
purpose. A row written by the old binary can still carry `source_key = NULL`,
which the partial idempotency index does not cover; only the legacy
`(recipient_agent_id, comment_id)` index identifies it, and the store recovers
that row's UUID through `GetLegacyCommentDelivery`. Pinning the conflict target
to the idempotency index would turn that recovery into a unique violation. The
generic collision this leaves open is closed at the domain boundary:
`Validate` rejects `Refs.CommentID` for any source type other than `comment`,
so a generic row can never occupy the comment projection key in the first
place.

The legacy mailbox list and daemon claim read only
`source_type='comment'`: their frozen DTO and rendered prompt require an
issue/comment, and a generic delivery must not fabricate either. Generic rows are
read through `GetDelivery` and claimed by the generic daemon entry below, which
has its own DTO and no issue fields.

## Atomic comment handoff

`CreateComment` resolves explicit mentions before writing. Resolution is
read-only, applies the existing permission and runtime checks, and removes
UI-suppressed targets. Inside the comment transaction it then:

1. inserts the comment;
2. inserts one delivery per valid external target through `delivery.Store`;
3. commits both, or rolls both back.

A mailbox insert failure (including the failure of the second recipient after
the first succeeded) rolls back the comment and all deliveries. The HTTP
response is an error, and no trigger outcome claims the handoff was queued.
There is no partial recipient boundary in this slice.

`blocked` and suppressed mentions keep their current semantics: a mention the
caller may not invoke stores no delivery and is reported as blocked after the
comment commits; an unchecked target stores nothing and produces no outcome.
Outcomes are computed only after the transaction commits, so `queued` for
`dispatch_kind=mailbox` means the request is durable.

Quick actions render an ordinary comment carrying the target mention and use
the same pre-resolution and transaction boundary, so they inherit the atomic
behavior without a separate write.

`UpdateComment` keeps the same pre-resolution and delivery boundary but the two
edit shapes order cancellation differently. A strict edit (`content_base` /
`expected_revision`) plans triggers first and then takes the conditional update;
only after that update wins does the transaction cancel tasks built from the old
body, write the new content and insert one delivery per valid external target. A
legacy non-strict edit still cancels tasks built from the old body *before* the
write transaction, so those cancellations are not inside it — only the new
content and the delivery inserts commit together. Attachment-set edits include
the delivery insert in the same transaction as well.

A mailbox failure on an edit rolls the content change back, so the response is
an error and the stored comment still has its old content. Re-saving the same
content therefore resolves the mentions again and can recover the handoff,
instead of silently deduplicating against the already-committed text. A
multi-recipient edit has the same all-or-nothing boundary as creation, and
suppressed or permission-blocked targets still store nothing and are reported
per target after the transaction commits.

## Consuming a generic delivery

Two entries consume the one table and their filters are disjoint, so the same
row can never be leased twice:

| Entry | Filter | DTO |
| --- | --- | --- |
| `POST /api/daemon/mailbox/claim` (legacy) | `source_type = 'comment'` | Frozen `MailboxMessageResponse` + rendered comment notification |
| `POST /api/daemon/deliveries/claim` (generic) | `source_type <> 'comment'` | `id`, `kind`, `payload`, recipient, bound session, lease and `verify_only` |

`delivery.Queue.Claim` leases for either entry. It takes a per-recipient
transaction-scoped advisory lock, leases at most one row per recipient session,
and only leases a row that is still eligible (`pending` due, `leased` expired, or
`unknown` due). The candidate list is only a pre-lock hint: the conditional
`UPDATE` re-checks both eligibility and the recipient's in-flight rows after the
lock, under READ COMMITTED's fresh statement snapshot. A lost race is a no-op,
and the comment and generic entries cannot each lease a row for the same session
even though neither candidate list contains the other projection's rows. This
keeps two workers or daemons from delivering to one session at the same time and
stops one unreachable recipient from consuming more than one slot. The worker
runs a bounded number of sessions concurrently; the claim itself is the fairness
bound, not a priority scheduler.

The issue-detail projection (`ListIssueMailboxDeliveries`) also keeps the
`source_type = 'comment'` filter. A generic delivery may legitimately reference
an issue, but the frozen `IssueResponse` mailbox DTO cannot render one, so a
generic row never leaks into that list.

`delivery.Queue.RecordOutcome` stores the fenced result. The delivery token,
daemon and unexpired lease fence the write, so a stale or foreign ack matches no
row. A delayed ack is retried by the worker under the same lease instead of
causing a second host send.

### Host contract

```text
DeliverIfIdle(target, delivery) -> host receipt | busy | unavailable | unsupported | rejected | unknown
LookupReceipt(target, delivery_id) -> accepted | pending | absent | error
```

Both calls address the exact registered session and the stable delivery id.
They never create, resume, archive or replace a session, and `DeliverIfIdle` has
no ordinary-send fallback. The Paseo adapter maps `paseo agent receipt
<session-id> <delivery-id> --json` onto `LookupReceipt`; an older CLI or host
that lacks the capability is reported as unsupported, not as an absent receipt.

### Outcomes and retry schedule

| Host result | Stored state | Delay | Next action |
| --- | --- | --- | --- |
| accepted | `delivered` | — | terminal; receipt saved |
| busy | `pending` | 5 seconds | same id delivered again |
| unreachable / missing capability | `pending` | failure backoff | same id retried; limitation stays in `last_error` |
| explicit rejection | `failed` | — | terminal; needs a human fix |
| unknown | `unknown` | failure backoff | receipt lookup only, never a resend |

The failure backoff is `attempts² × 15s`, bounded to 15 seconds … 15 minutes.
Only a proven no-effect result returns a row to claimable delivery. A row whose
lease expired without a recorded outcome is ambiguous: on reclaim the worker
looks up the receipt instead of sending again (the legacy comment projection
keeps its existing replay behavior because its frozen consumer has no receipt
path).

An `unknown` row is reclaimed for verification only. `accepted` completes it;
`pending`, `absent`, an unsupported lookup or a failed lookup all leave it
unconfirmed with a diagnostic. An absent receipt is explicitly not treated as
proof of non-execution, and recovery never changes the delivery id, clears the
receipt, or clears history.

A lease the daemon cannot use is a proven no-effect failure: the host was never
called. This covers a missing host session id, a foreign daemon binding, an
unparseable lease expiry and a second row for one session in a malformed batch.
The worker records a `pending` retry with a diagnostic immediately instead of
leaving the row leased until it is reclaimed for verification, which would turn
a never-sent delivery into a permanent `unknown`. When such a lease was already
a verification-only reclaim, the row stays `unknown` with the diagnostic,
because the earlier attempt may have been accepted; it never returns to
claimable delivery.

An `unknown` row has no automatic replay path. It is identified by its
`delivery_status`, the preserved delivery id and the diagnostic in `last_error`
or `host_receipt`; resolving it is a manual, out-of-band operator action against
that stable id. No API or UI action changes its state yet, and no recovery path
may change the id, clear the receipt, or resend on a guess.

## Failure rules

| Situation | Result |
| --- | --- |
| Comment or delivery insert fails | Neither exists; caller gets an error |
| Comment edit or its delivery insert fails | Old content stays; caller gets an error and can retry |
| Same key, same content | Original delivery ID; one row |
| Same key, different content | `ErrConflict`; original row untouched |
| Generic request with `Refs.CommentID` | Rejected by `Validate`; nothing stored |
| Unknown delivery ID in the scope | `ErrNotFound` |
| Stale or foreign lease acknowledgement | Fenced ack rejects it; a later lease's result is not overwritten |
| Busy session | `pending` with a short retry class; not a failure |
| Host without if-idle delivery | `pending` with a dependency error; no ordinary-send fallback |
| Ambiguous host result | `unknown`; receipt lookup only, never a resend |
| Receipt absent or lookup unsupported | Stays `unknown` with a diagnostic; not replayed |
| Claim proves the host was not called (missing session, foreign daemon, unusable lease) | `pending` with a dependency retry; neither sent nor parked as `unknown` |
| Unusable lease on a verification-only reclaim | Stays `unknown` with a diagnostic; never returns to claimable delivery |
| Worker crash after acceptance | Lease expires; the reclaim verifies the original id instead of resending |

## Rolling upgrade

Old binaries still list and claim without the `source_type='comment'` filter,
and their `SenderType` scan cannot read a NULL. No generic producer ships yet,
so they are safe today. Before one does, every daemon must be on a binary with
the read filter (or the fleet must be drained first); otherwise an old daemon
could claim a generic row and render an issue/comment notification that does not
exist. Old binaries also continue to write comment rows with `source_key = NULL`;
the new store recovers those by the legacy key, but a row must not be rewritten
or backfilled by hand during the rollover.

A new daemon can run against a server without the generic claim entry: the
request returns 404, the worker treats it as "not rolled out yet" and keeps
serving the legacy mailbox loop. The reverse order is not safe until the fleet
filter is in place. Old daemons never write `unknown`; the new state is only
produced by the generic worker, and its migration keeps the old constraint
values valid.

## Known gaps

- No CI watcher produces generic (`source_type != 'comment'`) deliveries yet;
  the worker and its claim/outcome entry are ready for one.
- The mailbox DTO has no generic projection; `GetDelivery` is the internal
  read path and the generic claim DTO is the daemon's.
- `unknown` rows have no API or UI recovery action yet; they are visible through
  `delivery_status`, `last_error`, `host_receipt` and the preserved delivery id,
  and are resolved manually as described above.
- Broadcast/multi-target fan-out, message batching, a second identity system
  and a separate service/outbox are out of scope.
