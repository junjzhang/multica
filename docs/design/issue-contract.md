# Issue contracts

A contract is an explicit, versioned delivery agreement on an issue. A delivery
is evidence submitted by its executor against one version. An acceptance is a
decision by that version's acceptor about named criteria. A return ends that
submission's review and sends the remaining work back to the executor. Neither
run completion nor mailbox processing constitutes acceptance.

The issue's assignee remains its executor, and its coordinator is its acceptor.
The existing merge authorization remains the sole merge policy. Creating or
revising an agreement requires a human workspace member, as does granting merge
authorization today. Agents can read agreements, submit deliveries, and decide
acceptance when explicitly named as acceptor. Executor and acceptor must differ.
This adds no authority to change merge policy or delegation return routing.

`issue.acceptance_criteria` owns the current criteria. A single issue-scoped
contract record stores immutable version snapshots and append-only submissions
and decisions. Snapshots retain the identities and merge policy they applied to;
changing those native fields makes the agreement stale until a human revises it.
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
