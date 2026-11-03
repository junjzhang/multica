# Managed delegation result execution and resource retention

A result-handling claim is a new task in the original conversation. Its
`delegation_result` identifies the saved delegated result and the accepted
runtime, provider, session, and working directory. The daemon verifies those
fields against the claim and passes `RequireResume` to the provider. It does
not look up a later source task or choose the latest session.

A strict turn must reuse the accepted directory under its env-root lock. A
missing directory, changed ownership, unsupported source environment, or
provider refusal before input produces `delegation_result_unavailable`.
Once provider input may have happened, an unsuccessful or ambiguous outcome
produces `delegation_result_input_unknown`. A nominal success without a reported
session address also remains unknown; prior hints are never filled in as receipt
evidence. Neither outcome permits a fresh
prompt retry. A post-prompt resume rejection cannot override this rule.
Busy directories and provider stores keep the same task waiting with its
preparation lease; cancellation ends that wait. A provider's explicit transient
refusal with `StrictResumeUnmet` also proves no input occurred. `StrictResumeUnmet`
alone is only an identity failure and can describe an error after input; it must
remain unknown rather than authorize resource release. That combination
waits in the same task and retries the same strict session; an ambiguous or
post-input failure never takes this path. Starting a task or observing
a session status event is not evidence that the result input was accepted.

The prompt references the saved result and asks the original conversation to
assess it. It does not repeat the delegated executor's instructions. Task
credentials, MCP credentials, and current authorized configuration belong to
the new task. A retained session is not permission to reuse the source task's
closed credentials.

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
S rather than creating a store keyed by R. A deleted business scope cannot be
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
