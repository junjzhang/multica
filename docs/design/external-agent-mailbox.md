# External agent sessions and mailbox delivery

Multica owns the issue, explicit handoffs, and delivery receipts. An external host owns its existing session and execution. A developer can mention an external reviewer on an issue, the reviewer can reply under its own agent identity, and either can escalate to the issue's coordinator. Neither an ordinary reply nor a status change schedules another turn.

## Identities and ownership

- `execution_kind=managed` uses the Multica runtime. `external` binds an agent to one existing host session; it never creates a replacement session. Only an omitted legacy field defaults to managed. Unknown values are non-executable.
- The session binding is unique by workspace, owner, provider, host ID, and session ID. The current provider is Paseo. Host ID means the Paseo server ID; session ID means the Paseo agent ID.
- The registered Multica delivery daemon must belong to the session owner. The adapter checks the connected Paseo server ID before sending.
- External credentials identify an agent, not a human or a fabricated managed run. Tokens are expiring and revocable, stored hashed, and bounded by the agent owner's current workspace access. Human-only operations remain unavailable.
- The issue coordinator is the original dispatch actor until explicitly changed. Reassignment and mentions do not transfer it. Member coordinator IDs are user IDs, not membership-row IDs.
- Merge authorization records a human-granted policy, defaulting to disallowed. It is not a merge executor and cannot constrain arbitrary Git credentials held by an external session. Review and current-head CI remain required before an authorized merge.

## Explicit handoff

Comments remain the canonical conversation. A parsed explicit agent mention routes to a managed run or an external mailbox according to the recipient's execution kind and invocation permissions. An agent may mention any permitted collaborator; there is no additional per-issue whitelist. Explicit assignment and rerun remain deliberate launch actions.

A disagreement is escalated by mentioning the coordinator with the disputed decision and evidence. Work waits for an explicit handoff to a subsequent executor. Discussion, delivery/read receipts, and changing status do not authorize a new run.

## Mailbox and delivery

- Each mailbox row indexes an issue comment and recipient. It stores delivery and processing state, not a second mutable conversation.
- Delivery: `pending -> leased -> delivered`, with retry or terminal failure. Leases and conditional acknowledgements prevent stale delivery workers from committing a newer worker's outcome.
- Processing: `unread -> read -> handled`. Advancing processing does not imply delivery success or completed implementation.
- A claim is scoped to the authenticated daemon. Owner PAT/JWT clients also provide `X-Multica-Daemon-ID`; the server verifies the selected registered daemon's ownership. The selector does not confer separate device credentials.
- The mailbox UUID is the host idempotency key. Notification text contains stable workspace, issue, comment, and recipient references; mutable titles and comment bodies are fetched by the recipient. Editing a comment cannot change a retry's payload fingerprint.
- A known busy host retains the request for a later attempt. A known pre-send dependency error is retryable. An ambiguous outcome is reported rather than blindly rerun. A terminal local result survives server acknowledgement failure and is re-acknowledged without another host send.

Issue detail includes external participants, session links, and recent delivery receipts. The workspace-scoped `issue_mailbox:changed` event invalidates only the affected issue detail after committed mailbox changes; it carries no issue snapshot and does not trigger execution. Partial list/search/event payloads omit fields they did not load. Explicit `null`, `[]`, and `allowed:false` remain authoritative clearing values.

## Register an existing session

Use an owner-authenticated profile for registration. Obtain `serverId` from `paseo status --json`, the existing session ID from Paseo, and the owner daemon ID from `multica runtime list --output json`.

```sh
multica --profile owner agent external register \
  --name 'External reviewer' --provider paseo \
  --host-id '<paseo-server-id>' --session-id '<existing-paseo-agent-id>' \
  --daemon-id '<owner-multica-daemon-id>' \
  --url 'paseo://h/<paseo-server-id>/agent/<existing-paseo-agent-id>'

multica --profile owner agent token create '<agent-id>' \
  --expires-at '<RFC3339-expiry>' --save-profile external-reviewer

multica --profile external-reviewer agent mailbox list '<agent-id>' --output json
multica --profile external-reviewer agent mailbox ack '<agent-id>' '<message-id>' --state read
multica --profile external-reviewer issue get '<issue-id>' --output json
multica --profile external-reviewer issue comment add '<issue-id>' --content-stdin
multica --profile external-reviewer agent mailbox ack '<agent-id>' '<message-id>' --state handled

multica --profile owner agent token revoke '<agent-id>' '<token-id>'
```

The external session must use its agent profile when reading and replying; it must not fall back to the owner's human profile. Automatic registration and MCP are outside this version.

## Paseo integration dependency

The Multica adapter expects a separate, atomic host operation:

```sh
paseo send '<existing-session-id>' --if-idle --message-id '<mailbox-uuid>' \
  --prompt-file '<notification-file>' --no-wait --json
```

This host extension is **not included in this repository**. The currently deployed Paseo CLI does not support it. Normal `send` can interrupt or steer, so the adapter must never fall back to that command. Unsupported hosts leave a visible retryable dependency error; mailbox reads alone do not wake sessions.

The host extension must use a distinct wire request that older servers reject, atomic busy admission, and durable message receipts. It must preserve the same existing session and return a known-no-effect busy result without interrupting the current turn. Real-host idle wakeup, busy deferral, and crash/reconnect acceptance remain pending until that extension is implemented and tested.
