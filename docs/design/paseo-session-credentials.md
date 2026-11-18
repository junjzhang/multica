# Paseo session identity and credentials

WORK-518 / WORK-519

## Identity and issuance

Multica retains the identity registered for a Paseo `(provider, host, session)` under its workspace and owner. Issue bindings, comments, contracts, and delegation return addresses continue to refer to it. Paseo owns the conversation lifecycle. An administrative `revoked_at` can still block issuance for a compromised or banned identity.

On first business use, Multica CLI calls the local `session.connect` plugin RPC. The plugin resolves the host and verifies the session exists and is unarchived before looking up or registering its identity. It then requests a session-bound `mea_` token from Multica using the configured owner profile. The RPC returns safe status metadata and the plugin atomically writes the session credential to the existing private v1 cache. The configured owner credential stays in the daemon-side API path.

The CLI reads the cache directly when the token has more than five minutes left. In the default cache, a missing token or one with at most five minutes left triggers the same RPC. Renewal failure stops the command and surfaces a recovery hint. A session restored from archive can renew against its original identity. Previously minted credentials naturally expire. The manual `connect` command can request a credential immediately and can repair a remotely revoked cache entry. Managed tasks continue to use `mat_` and explicit owner administration uses the existing owner client.

## Lifetime and measurement

Tokens expire after one hour. The CLI and plugin use a five-minute renewal window. A local benchmark used Paseo 0.15.3 with a temporary home and Unix socket. With the daemon and example plugin already running, 20 fresh `paseo plugin rpc` processes invoking `increment` succeeded. Median was **862 ms**, P95 **902 ms**, and maximum **945 ms**. This measures CLI startup and local RPC. Multica API latency is additional. A five-minute window leaves room for the RPC's 60-second CLI deadline and manual retry while an active session typically needs only one renewal per 55 minutes.

The server sweeps up to 1,000 expired external token rows every minute in a bounded transaction. Expiry already makes those tokens unusable at authentication time. The sweep includes expired revoked rows and leaves external session identities intact.

## Board and delivery

The plugin uses its owner profile to read both board views. The session view resolves an exact registry record and passes its record ID to `session-tasks`. An authorized human can select a session belonging to the readable agent in the same workspace. A session credential continues to use the session from authentication. An unregistered conversation has an empty session view and can still open the workspace view.

Atomic if-idle delivery treats `agent_archived` as a terminal failure and stores the reason on the delivery. The board shows recent archived failures under **需要你**, including deliveries attached to completed issues. The session view filters by the registered source or target. **在 Paseo 中打开** navigates to the frozen target. Restore the conversation in Paseo and initiate a new delivery manually.

## Rollout and migration

1. Roll out the backend, plugin and CLI together, then validate first use, owner board reads, and renewal in a test conversation.
2. Prepare an inventory of old 90-day session tokens and their corresponding local binding files. Confirm the revocation list with the operator. Revoke the approved tokens and remove those old cache files. Retain their session identity records. Active conversations obtain fresh tokens on their next CLI call.
3. Confirm the six legacy per-session agents and two suffixed test templates with the operator. Archive the approved records after checking their active references. Keep their histories and registry rows.
