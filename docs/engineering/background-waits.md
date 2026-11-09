# Background command waits

An external Paseo session or a managed task can submit a bounded command to its local Multica
daemon and finish its turn. Waiting runs ordinary processes and makes no model
calls. The daemon sends one terminal result through the existing Mailbox, with
the original registered session frozen as its destination. Busy sessions wait;
unknown send outcomes use the original message's receipt lookup.

## Use

Connect the current Paseo session first (`multica session status`). Start an
updated daemon using its existing profile, then run:

```bash
multica wait start --daemon-profile trial --timeout 2h -- gw mr checks 13 --watch
multica wait get --daemon-profile trial <wait-id>
multica wait cancel --daemon-profile trial <wait-id>
multica agent mailbox list <agent-id> --kind background_wait --output json
```

Inside a managed task, omit `--daemon-profile`:

```bash
multica wait start --timeout 2h -- gw mr checks 13 --watch
multica wait get <wait-id>
multica wait cancel <wait-id>
```

The injected task credential and daemon port select the active execution.
The command directory must be inside that task's real work directory. The
daemon verifies its active token registry and the API verifies the current
task, workspace and confirmed provider pin. No owner profile or external
credential is used. Providers must support strict session resume.

A managed task may have one unconsumed wait. Ending its provider turn with an
unconsumed wait suspends the task through the existing deferred queue; it does
not complete the task, notify its callers of completion, or release its worktree.
The journal persists and retries this suspension as well as the command result.
Results enter Mailbox and make the same task eligible for another claim. That
claim requires the original provider session and work directory, retaining the
task's accepted returns. Missing original resources fail visibly rather than
starting a fresh session. The result is included in the resumed prompt.

`wait get` on a terminal result durably consumes it for the current task. When
the running provider has already read the result, ending that turn follows normal
completion and creates no extra result-handling turn. A running-status query
does not consume anything. Other tasks cannot read or cancel this wait.

`wait start` prints JSON immediately after durable registration. It also prints
the request ID and absolute deadline to stderr before submitting. If a response
is lost, repeat the same command, cwd, `--request-id` and `--deadline`:

```bash
multica wait start --daemon-profile trial \
  --request-id <original-uuid> --deadline <original-rfc3339-deadline> \
  --cwd /path/to/repo -- gw mr checks 13 --watch
```

The first accepted request wins. A duplicate returns its existing status;
different content under that ID is rejected. Use argv directly; shell syntax
requires explicitly invoking a shell. The command inherits the daemon's normal
environment and tool configuration, with `MULTICA_*` credentials/context removed.
Do not put credentials in command arguments. The caller's environment and API
token are never persisted or injected into the command.

Timeout defaults to one hour and is limited to seven days. At most 32 commands
run concurrently. Logs contain the first 1 MiB of combined output; further
output is drained and discarded. Results include status, exit code, finish time
and the private local log path. Raw output and command arguments are never
included in Mailbox messages. Treat tool logs as private: tools can print secrets.

## Cancellation and recovery

Cancellation durably records `cancelling`, stops the local process tree and then
records `cancelled` with its exit code. It does not cancel the remote CI/job.
Cancellation after a terminal result returns that same result. The first terminal
decision wins completion/cancellation races. Timeout records `timed_out`.

Automatic reload and CLI upgrades defer while any wait process is active.
The same admission barrier prevents a new wait from starting once an upgrade
has committed to restart. Explicit daemon shutdown/restart still interrupts
unfinished waits and reports `lost`.

The daemon owns its profile's private `waits/` journal and
`wait.endpoint.json` capability. It commits the terminal result before posting
it to `/api/daemon/background-waits/complete`. A failed request or lost response
replays the identical result using `background_wait:<wait-id>` as notification
identity; the existing Mailbox returns the original message ID. A restart never
reruns commands. Any unfinished journal becomes `lost` and is notified. Terminal
results waiting for API acceptance are retried; `notify_error` makes that visible.
`delivery_id` means queued in Mailbox, not received or handled by the session.

A small subprocess in the same binary watches a pipe owned only by the daemon.
If the daemon dies, EOF cancels and reaps the command's process group / Windows
Job Object. Commands must not deliberately escape process-tree containment or
detach into independent services. Journals and logs remain until the owner
removes them; there is no automatic command replay or retention policy.
An unreadable or invalid journal stops daemon startup rather than guessing its
return address. The owner must preserve and repair or move that file before
restarting; moving it does not authorize replaying its command.

## Authority and verified boundary

Execution is available only through the loopback endpoint and a random capability
in the owner's private local profile. An API/session UUID alone grants no
execution capability; there is no remote command queue. Session authorization
reuses the authenticated external agent, exact registered session, workspace
membership and daemon owner. The target must be a Paseo session registered to
that daemon. Managed task tokens authorize only their active task and work
directory; they cannot choose an external return address. External `get` and
`cancel` require the local owner capability; managed operations use the task token.

Processes sharing an OS account are not isolated from that account's readable
files; this is the same local owner boundary as the daemon profile. Commands run
with the daemon owner's OS privileges. `--daemon-profile` selects only the local
endpoint; it never changes the external session's API identity or credentials.
The server completion endpoint authenticates the executing daemon/owner and
workspace. Subsequent session rebinding cannot retarget an accepted result.
Archive/revocation/membership checks remain in the existing delivery claim path.

Tests use isolated databases, test-created commands and a fake Paseo executable.
They do not exercise production sessions, actual CI tools, model calls or shared
service deployment. No production migration or new server table is required.
