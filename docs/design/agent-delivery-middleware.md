# Agent delivery middleware: first-slice contract

This document fixes the smallest production contract for the delivery core
extracted from the external-agent mailbox. It describes the domain the core
owns, how it maps onto the existing `agent_mailbox_message` table, and the
business writes that are now atomic with enqueue. It is not the full event and
wakeup middleware design; sources, host adapters and scheduling stay outside
this slice.

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

Deliberately unchanged in this slice:

- Daemon claim/lease/acknowledgement behavior and its rendered notification.
- Paseo/host adapters and the external-agent CLI.
- `IssueWakeupService`, autopilot scheduling and CI watchers.
- The frozen mailbox API DTO and the external-agent identity/permission model.

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
issue/comment, and a generic delivery must not fabricate either. Generic rows
are stored and readable through `GetDelivery`; exposing them to a host is a
later adapter slice.

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

`UpdateComment` uses the same shape for a content edit. It resolves the new
content's mentions after cancelling tasks built from the old body (the ordering
the legacy path already used), then writes the new content, the cancellations
and one delivery per valid external target in one transaction. Strict edits
(`content_base` / `expected_revision`) and the legacy non-strict edit share
that boundary; attachment-set edits include the delivery insert too.

A mailbox failure on an edit rolls the content change back, so the response is
an error and the stored comment still has its old content. Re-saving the same
content therefore resolves the mentions again and can recover the handoff,
instead of silently deduplicating against the already-committed text. A
multi-recipient edit has the same all-or-nothing boundary as creation, and
suppressed or permission-blocked targets still store nothing and are reported
per target after the transaction commits.

## Failure rules

| Situation | Result |
| --- | --- |
| Comment or delivery insert fails | Neither exists; caller gets an error |
| Comment edit or its delivery insert fails | Old content stays; caller gets an error and can retry |
| Same key, same content | Original delivery ID; one row |
| Same key, different content | `ErrConflict`; original row untouched |
| Generic request with `Refs.CommentID` | Rejected by `Validate`; nothing stored |
| Unknown delivery ID in the scope | `ErrNotFound` |
| Stale or foreign lease acknowledgement | Existing fenced ack rejects it; a later lease's result is not overwritten |
| Ambiguous host result | Existing daemon contract reports it; this slice does not auto-replay it |

## Rolling upgrade

Old binaries still list and claim without the `source_type='comment'` filter,
and their `SenderType` scan cannot read a NULL. This slice has no generic
producer, so they are safe today. Before any generic producer ships, every
daemon must be on a binary with the read filter (or the fleet must be drained
first); otherwise an old daemon could claim a generic row and render an
issue/comment notification that does not exist. Old binaries also continue to
write comment rows with `source_key = NULL`; the new store recovers those by
the legacy key, but a row must not be rewritten or backfilled by hand during
the rollover.

## Known gaps

- The daemon does not claim generic (`source_type != 'comment'`) deliveries
  yet, and no CI watcher produces them.
- The mailbox DTO has no generic projection; `GetDelivery` is the internal
  read path.
- Broadcast/multi-target fan-out, a second identity system and a separate
  service/outbox are out of scope.
