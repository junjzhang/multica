# Background waits

Use `multica wait start --description "Verification" --issue WORK-123 --timeout 3h -- <command>`
only when command execution is authorized. Omit `--issue` in managed tasks: the
associated issue comes from the task. `started_at` is persisted at registration
and does not reset on daemon restart; absent legacy metadata stays absent.

Use `multica wait list --daemon-profile <local-profile>` to inspect waits without
consuming results, ACKing Mailbox messages, creating waits or cancelling commands.
Managed tasks omit the profile and use their injected task credential and daemon
port. External sessions see only waits targeted to that exact registered session;
owner context sees the local daemon's records. A profile selects the local daemon,
not a different API identity. Task credentials cannot query arbitrary sessions.

Filter with `--session <session-row-uuid>`, `--workspace <uuid>`, `--task <uuid>`,
`--state running,cancelling`, `--delivery-status pending,unknown` or
`--processing-status unread,read`. These filters can only narrow credential scope.
Use `--limit 1..200` and `--cursor <next_cursor>` with unchanged filters; snapshot
changes require restarting pagination. `snapshot_version` identifies content;
`observed_at` and optional `delivery_observed_at` describe its observation times.
The list omits full command arguments, working directories and log paths.

Execution completion, Mailbox insertion, host delivery and processing are separate
facts. `delivery.enqueued` means insertion, `delivery_status=delivered` means host
acceptance, `processing_status=handled` means processing receipt (or managed result
consumption). `known=false` means receipt freshness could not be verified; it is
not the same as a server-observed `delivery_status=unknown`. Failed receipt reads
must not be interpreted as no waits or handled results. Local notify retries and
`lost` after daemon restart remain visible. Listing never replays a command.

For bridges, use the same local `GET /waits` snapshot interface. It hydrates receipts
through a read-only, workspace-scoped batch API. Local query parameters match the
CLI flags, with underscores for `delivery_status` and `processing_status`. Never refresh a UI by calling
managed `wait get`: terminal get consumes the result. External `get` does not ACK
Mailbox. Processing receipt and task acceptance remain separate operations.
