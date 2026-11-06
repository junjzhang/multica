# Issue contracts

A contract is an explicit, versioned delivery agreement on an issue. A delivery
is evidence submitted by its executor against one version. An acceptance is a
decision by that version's acceptor about named criteria. A return ends that
submission's review and sends the remaining work back to the executor. Neither
run completion nor mailbox processing constitutes acceptance.

The issue's assignee remains its executor, and its coordinator is its acceptor.
The existing merge authorization remains the sole merge policy. A human member other than the executor may define the agreement. The immutable
original request agent may also maintain its issue agreement within verified
originator/workspace/business scope. A coordinator label or a return address
alone grants no editing authority. Agents can read agreements, submit deliveries,
and decide acceptance when explicitly named as acceptor. Executor and acceptor must differ.
The versioned authorization_note records agreed scope; it is not a machine execution grant. It cannot grant API access, merge or deployment rights, or bypass dedicated permission checks. Structured merge policy always comes from the native issue field.
This adds no authority to change merge policy or delegation return routing.

`issue.acceptance_criteria` owns the current criteria. A single issue-scoped
contract record stores immutable version snapshots and append-only submissions
and decisions. Snapshots retain the identities and merge policy they applied to;
changing those native fields makes the agreement stale until an authorized requester revises it.
Acceptance never crosses versions, including when wording is unchanged.

Contract operations serialize on the issue row. An expected sequence protects
concurrent edits, and a caller-generated request UUID makes retries idempotent;
reusing it with different content or identity conflicts. Only one undecided
delivery may exist for a version. Accepted criteria accumulate within a version.
Partial acceptance returns remaining work to the executor. All criteria accepted
moves the native issue status to done. Cancellation stays distinct. Direct done
writes, including PR automation and workflow moves, cannot bypass acceptance on
contracted issues. Issues without contracts retain their existing behavior.

An acceptance may explicitly close listed direct delivery/review children only
when they were included in that submission, remain in_review, share its acceptor,
and have no separate contract. Other children retain their statuses. This is not
a recursive close operation. Closing and the acceptance commit atomically.

The shared Web/Desktop issue sidebar places the expanded contract above external
collaboration. Server data uses Query; no second client state store is introduced.
The CLI uses the same endpoints. Agent context includes only the current summary
and an API/CLI pointer to history, rather than injecting historical agreements.

## API and CLI

`GET /api/issues/{id}/contract` returns the agreement's versions, deliveries,
decisions, current accepted indices, pending identity and caller capabilities.
`GET /api/issues/{id}` includes only its current version and pending delivery.
Absent contracts are null on the dedicated endpoint and omitted from issue
detail. An old client can ignore the additive field.

`POST /api/issues/{id}/contract` accepts one explicit action. All writes require
`client_request_id` (UUID) and `expected_sequence` (0 to create). Non-definition
actions also require `version`. Criterion indices are zero-based within that
version. The response is the refreshed contract view; 409 means the agreement,
delivery, identity or sequence changed, and 403 means the actor lacks authority.

```json
{
  "action": "revise",
  "client_request_id": "01900000-0000-7000-8000-000000000001",
  "expected_sequence": 0,
  "goal": "Ship the change",
  "scope": "Shared issue details",
  "authorization_note": "Implement and test; no merge",
  "requirements": ["API tests pass", "Browser evidence reviewed"]
}
```

```json
{
  "action": "submit",
  "client_request_id": "01900000-0000-7000-8000-000000000002",
  "expected_sequence": 1,
  "version": 1,
  "criteria": [0, 1],
  "evidence": "PR and test report URLs",
  "child_ids": []
}
```

```json
{
  "action": "accept",
  "client_request_id": "01900000-0000-7000-8000-000000000003",
  "expected_sequence": 2,
  "version": 1,
  "delivery_id": "01900000-0000-7000-8000-000000000002",
  "criteria": [0],
  "reason": "API verified",
  "child_ids": []
}
```

```json
{
  "action": "return",
  "client_request_id": "01900000-0000-7000-8000-000000000004",
  "expected_sequence": 4,
  "version": 1,
  "delivery_id": "01900000-0000-7000-8000-000000000005",
  "reason": "Include refreshed screenshots"
}
```

The CLI reads with `multica issue contract get ISSUE --output json` and applies
the exact JSON file with `multica issue contract apply ISSUE --request-file
./request.json`. Retry an uncertain write with the same request UUID and body.
A successful replay returns current state without appending another decision.
Changing request content or authenticated identity while reusing a UUID conflicts.

Definitions require 1–50 nonempty criteria (at most 2,000 bytes each), an explicit
goal and authorization, distinct current executor/acceptor, and an authorized author other than the executor. Write bodies are limited to 64 KiB; a contract's stored
history is limited to 2 MiB. Reaching the history limit rejects the write without
changing the issue. This initial slice has no history-pruning operation.

Contract transitions retain the executor and do not invoke workflow column
handoffs or start agent runs. A named project workflow must contain the native
in_progress and in_review statuses used by this slice; its normal membership
validation still applies. Done/closed behavior continues through the existing
issue status and wakeup mechanisms. Definitions do not modify workflow routing.

Dedicated permission revocation remains available on a completed issue. Such
administrative edits preserve its historical done status and make the agreement
stale. No further delivery or acceptance is allowed until an authorized requester
revises it; that revision reopens the native issue. The current structured merge
policy takes effect immediately, including while an agreement is stale.

Every semantic contract mutation uses the native issue writer and advances its
owner revision once, including note-only revisions. Resumed claims compare that
revision for contracted issues and require a fresh compact issue read when it
changes. Older comparison snapshots become unknown once, rather than waiving a
read of fields they never captured. Neither history nor a second lifecycle is
injected into resumed runs. Wakeup events use the authenticated actor and source
task, including note-only updates and suppression of the source's own wakeup.

Managed creator maintenance verifies an active authenticated run and the same
originator and business scope as the creation run in issue.origin_id. A scopeless
quick-create requires that exact run. External creator maintenance uses the mea
identity and its owner's current workspace membership, without fabricating a
task id. Missing provenance is refused; a new dispatcher does not acquire editing
rights from a coordinator change. Both current and versioned executors are
excluded, so changing assignee cannot let an executor revise its own agreement.
