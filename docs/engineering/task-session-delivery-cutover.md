# Task-scoped delivery cutover

## Release gate

Use this procedure for the task-scoped session and result-delivery contract in [task-session-delivery.md](../design/task-session-delivery.md). It is a maintenance-window upgrade of one deployment, not a rolling-upgrade compatibility promise. Development, rehearsal, and production acceptance are separate gates. This document does not authorize a production change.

Before booking the window, attach a release manifest to the deployment issue containing:

- Exact tested source commit, Draft PR, independent review, and required CI for that commit.
- Backend, frontend, CLI, delivery daemon, and host artifact identities. Record the backend's `/health.commit` and the daemon's reported CLI version separately; they can differ.
- Current database migration ledger and the candidate's ordered migration list, including concurrent-index completion checks.
- Deployment files, container/process identities, exposed ports, database instance, daemon profile, host server ID, and retained session/receipt storage. Inventory paths and hashes without copying credential-bearing environments into logs.
- A dedicated acceptance issue and existing host session, expected result recipient, and the exact evidence that proves one host acceptance and no duplicate managed execution.
- The maintenance admission fence, drain criteria, backup/restore rehearsal evidence, and the supported application rollback boundary.

An unreviewed source change, unexpected live version, unresolved migration collision, or missing required runtime evidence keeps the release unready. A green build does not replace the following gates.

## Rehearse in isolation

Create the environment through the repository's managed environment commands. Use a unique `MULTICA_DEV_HOME`, checkout database, ports, CLI profile, and daemon workspace. Never copy the production `.env` into a development checkout. Do not stop the shared development PostgreSQL service when stopping one test environment.

Use a consistent, access-controlled copy of production data for migration rehearsal. Keep copied tokens private, prevent copied schedulers/daemons from reaching real hosts, and use dedicated credentials and sessions for any real delivery. Test migrations against both an empty database and the copied existing state. Preserve an inventory of original delivery IDs, statuses, receipts, and binding evidence before and after migration.

Rehearsal must demonstrate that:

1. Every existing delivery remains accounted for. Migration does not drop a row or manufacture a new notification ID.
2. Delivered and processing receipts are preserved. Uncertain results retain the original receipt identity and cannot become ordinary sends.
3. Unsent legacy messages acquire a target only when the original destination can be established from reliable evidence. A current Agent binding alone is not historical proof.
4. Unresolved legacy rows remain visibly unready with a reason; they are not silently skipped, redirected, or reported as delivered.
5. Re-running a failed migration step does not duplicate bindings or notifications. Inspect the migration ledger and actual indexes; a recorded version alone is insufficient.
6. The candidate API, calling CLI, delivery worker, and host complete the accepted task/result/return-session flow. Include busy, lost acknowledgement, deletion, explicit takeover, and cancellation cases from the design contract.
7. A rollback rehearsal states exactly which application versions can read which data. Startup against an empty migrated database proves less than recovery with post-upgrade messages.

Store measured migration and drain durations with the rehearsal evidence. Do not promise a production window duration before measuring it.

## Freeze and drain

Identify every path that can create or claim work: interactive API clients, managed Agents using the API, comments and assignments, scheduled wakeups, autopilots, webhooks, and all registered delivery workers. Pausing a browser or one daemon is not a write fence.

Apply and verify the admission and claim fence before waiting for managed executions to finish. Preserve existing results and completion acknowledgements while draining. The daemon has no public drain operation: its internal claim barrier requires zero active tasks, while `daemon stop` and `/shutdown` cancel active execution contexts. Neither shutdown nor pausing individual producers establishes the required boundary.

A temporary PostgreSQL trigger fence is an operational option for the task queue and delivery leases. Qualify its scripts against both the actual deployed version and the candidate; include every consumer, including a legacy mailbox loop when present. Attach the exact scripts and hashes to the release manifest. Rehearsal scripts restricted to a test database are not production commands.

The reviewed fence must:

- Install both task and delivery guards atomically under table locks, at READ COMMITTED isolation, with bounded lock and statement timeouts. Read back the installed definitions; submission alone is not proof.
- Reject new transitions or inserts into dispatched/running/waiting task states, and new or replaced delivery leases. Already-admitted task transitions, heartbeats, result reports and existing-lease outcomes remain allowed.
- Preserve notification identities, targets and receipts. A due deferred task may legitimately become queued before its claim is refused; record that transition rather than claiming a complete write freeze.
- Offer an idle-only installation that checks for active tasks and leases while holding the same table locks. A failed check rolls back without installing either guard.
- Be removable atomically without cancelling tasks, replacing queued IDs, or resetting unknown deliveries. If a running parent waits for a newly blocked child, abort the maintenance window and remove the fence so the same work can continue.

Claim requests may return temporary errors while fenced. Do not describe that interval as healthy ordinary operation or leave the fence in place indefinitely. Count pre-fence dispatched/preparing tasks and leases as in flight, even if their provider call has not started. Unknown delivery state does not prove an external host is idle.

This fence does not cover direct cloud fleet commands, node execution, arbitrary plugin effects or work already accepted by an external host. Inventory those integrations, restrict their mutation ingress and drain admitted calls, or establish that they are unconfigured. Verify that no participating writer session bypasses the guards. If those conditions cannot be established, the deployment is not fenced; do not stop busy daemons or claim a completed drain.

After managed executions reach the agreed terminal boundary, stop/fence all message producers and claimants. Verify process state and database activity rather than trusting that a stop command was sent. Let ordinary HTTP transactions drain. Record all remaining leased or uncertain deliveries and reconcile available host receipts using their original IDs. Lease expiry is not proof that a send did not occur.

Take the final consistent backup and delivery inventory only after the write fence is verified. Preserve the deployed application artifacts and configuration references alongside the backup. Keep secrets out of the issue and test logs.

## Migrate and replace

Apply the reviewed migration artifacts while old readers and writers remain fenced. Complete and verify every concurrent index before starting candidate consumers. Recheck the operational guards after DDL and record whether the backup contains them. Do not reuse occupied migration numbers or declare success from the last migration's exit code alone.

Install the tested backend, calling CLI, and delivery daemon as one compatible set; rebuild/update the frontend for the new issue controls and schemas. Start and verify the upgraded backend before starting the upgraded daemon. The daemon requires `workspace_id` in each delivery claim response; an older backend leaves all such claims unusable and prevents delivery. The host must satisfy the idle-only delivery and receipt contract. Do not replace the host merely because the Multica application changed. Preserve host instance identity, existing sessions, receipts, daemon identity, and credential profiles.

Before reopening normal traffic:

1. Read back artifact identities, backend health, migration ledger, and worker/host capability. Reconcile them with the tested release manifest.
2. Keep ordinary queues fenced. If a qualification exception is needed, permit only an explicitly identified, newly created acceptance workspace with one operator, dedicated Agents/runtimes/claimants and no prior task history. Resolve task workspace ownership from the actual Agent record, never a caller-supplied context field. Keep external mutation ingress restricted. Qualify this selective fence and its reversal before using it in production.
3. Run the dedicated acceptance issue through managed execution and durable result submission. Verify that ordinary workspaces still produce no new provider inputs or delivery calls; ordinary queued, deferred and unknown records remain accounted for.
4. Verify that the original Leader session receives the expected notification once. Inspect the host receipt and task/comment records, then verify acceptance or rework through the intended UI/CLI.
5. Inspect the frozen inventory again. No uncertain delivery may have been reset into a fresh send, and no unrelated managed task may have been restarted. If acceptance fails, remove the qualification exception to restore the full fence; preserve the new records and follow recovery below.
6. Only after acceptance is approved and remaining dedicated acceptance work is accounted for, remove the reviewed queue guards atomically and reopen admission using the recorded pre-window settings. If queued acceptance work is not approved for release, stop its dedicated claimant after it is idle and verify its runtime is unavailable to other claimants before removing the global guards. Confirm the intended writer/consumer set and progress of the original queued IDs, with no new unclassified failures.

A session that no longer exists leaves a visible notification failure and a preserved result. Do not change its receipt address to make the acceptance counter look green.

## Abort and recovery

Before candidate business writes, the rehearsed rollback may restore the compatible previous application against the retained schema, or restore a backup only while all writers remain fenced and no newer results would be lost. The release manifest must state which option was actually tested.

After candidate writes, first fence new dispatch and preserve new tasks, results, bindings, deliveries, and receipts. Prefer correcting the candidate or a specifically verified compatible recovery build. Do not blindly downgrade to a binary that assumes one session per Agent. Do not restore the pre-upgrade database over new work.

Retry only an operation whose outcome is known. For an uncertain send or acknowledgement, look up the original host receipt. An explicit takeover creates a separately identified notification of the preserved result; it does not rewrite or erase the uncertain original.

Record the actual recovery action, preserved records, unresolved delivery IDs, and checks still outstanding. Cleanup of old fields, indexes, artifacts, or backups is a later change after the agreed acceptance period.
