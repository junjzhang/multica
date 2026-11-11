# External agent template cutover

Maintenance-window procedure for the template, task-leadership and archive
semantics in [external-agent-templates.md](https://github.com/junjzhang/multica/pull/18)
(design PR #18, commit `533958f8`). This document updates deployment, migration
and rollback for slices 1-3; it does not authorize a production change. The
drain, fence, backup and artifact-identity mechanics are the ones in
[task-session-delivery-cutover.md](task-session-delivery-cutover.md); this page
adds what the template switch and the external-agent archive add on top.

## Gate

Book the window only with a release manifest containing:

- tested source commit and PR, review verdict, and the checks that actually ran;
- migration ledger and the candidate's ordered migrations, including the
  concurrent-index steps (`655`-`661` at the time of writing);
- backend, web/desktop, CLI, plugin and daemon identities;
- the template switch value before and after (`MULTICA_EXTERNAL_AGENT_TEMPLATES_ENABLED`);
- every squad whose `leader_id` is an external agent, with its members,
  runtimes, open tasks and contracts;
- the acceptance squad, two registered host sessions and the evidence that each
  session can lead its own task chain without cross-drift;
- the rollback option that was rehearsed.

## Preflight (read-only)

Run before the window. Record exact ids, not prefixes.

```sql
-- Legacy external defaults that must be migrated by a human.
SELECT s.id, s.name, s.leader_id, a.name AS leader_name
FROM squad s JOIN agent a ON a.id = s.leader_id
WHERE s.archived_at IS NULL AND a.execution_kind = 'external';

-- External agents with any in-flight reference (archive gate).
WITH refs AS (
  SELECT 'in_flight_tasks' AS code, t.agent_id, t.id::text AS ref FROM agent_task_queue t
   WHERE t.status IN ('queued','dispatched','running','waiting_local_directory','deferred')
  UNION ALL
  SELECT 'messages', m.recipient_agent_id, m.id::text FROM agent_mailbox_message m
   WHERE m.delivery_status IN ('pending','leased','unknown') AND m.withdrawn_at IS NULL
  UNION ALL
  SELECT 'returns', r.dispatcher_agent_id, r.id::text
   FROM issue_delegation_return r JOIN issue i ON i.id = r.issue_id
   WHERE r.is_current
     AND (i.status NOT IN ('done','cancelled') OR EXISTS (
       SELECT 1 FROM agent_mailbox_message m
       WHERE m.source_type = 'delegation_result' AND m.processing_status <> 'handled'
         AND m.withdrawn_at IS NULL
         AND m.payload->>'delegation_return_id' = r.id::text))
  UNION ALL
  SELECT 'assigned', i.assignee_id, i.id::text FROM issue i
   WHERE i.assignee_type = 'agent' AND i.status NOT IN ('done','cancelled')
  UNION ALL
  SELECT 'leader', i.leader_agent_id, i.id::text FROM issue i
   WHERE i.status NOT IN ('done','cancelled')
  UNION ALL
  SELECT 'squad_leader', s.leader_id, s.id::text FROM squad s WHERE s.archived_at IS NULL
  UNION ALL
  SELECT 'squad_member', sm.member_id, sm.id::text FROM squad_member sm
   WHERE sm.member_type = 'agent'
)
SELECT a.id, a.name, a.execution_kind, r.code, count(*) AS refs
FROM refs r JOIN agent a ON a.id = r.agent_id
WHERE a.execution_kind = 'external'
GROUP BY a.id, a.name, a.execution_kind, r.code
ORDER BY a.id, r.code;

-- Legacy wide credentials that name no session.
SELECT id, agent_id, workspace_id FROM external_agent_token
WHERE external_session_id IS NULL AND revoked_at IS NULL;

-- Contracts that still name an external executor, acceptor or squad
-- representative. The executed basis has no execution_kind field, so resolve
-- each named actor against agent.execution_kind instead.
SELECT c.issue_id, a.id AS agent_id, a.name, a.execution_kind
FROM issue_contract c
JOIN agent a ON a.id IN (
    NULLIF(c.data->'versions'->-1->'executor'->>'id','')::uuid,
    NULLIF(c.data->'versions'->-1->'acceptor'->>'id','')::uuid,
    NULLIF(c.data->'versions'->-1->'basis'->'executor_representative'->>'id','')::uuid)
WHERE a.execution_kind = 'external';
```

Classify every hit as migrate, hand off, or leave with its owner. A host session
that cannot be upgraded keeps its old Agent and is parked; do not open
multi-session writes for it.

The archive preview endpoint returns the same blocking categories plus the
notifications the archive withdraws:

`GET /api/agents/{id}/archive-preflight` →

`{ "agent_id": "...", "archivable": false,
   "blockers": [{ "code": "in_flight_tasks", "blocking": true, "count": 2,
                  "sample_ids": ["..."] }] }` with codes
`in_flight_tasks`, `active_waits`, `unaccepted_contracts`,
`unverified_messages`, `current_returns`, `assigned_issues`,
`task_leaderships`, `coordinated_issues`, `squad_default_leader`,
`squad_memberships`, and the non-blocking `pending_messages`.

- `current_returns` counts an accepted return relationship only while its issue
  is open or a result it produced is still unhandled. A closed issue keeps the
  current row for history but is not an archive obligation.
- `pending_messages` lists exactly the rows the archive transaction withdraws
  (delivery target set). Legacy rows with no target are preserved untouched and
  are not reported as withdrawn.
- `unaccepted_contracts` matches the executor, the acceptor and the squad
  `executor_representative` of the current version.
- Archiving an external Agent is a human owner/admin action; machine credentials
  are refused even when they can manage the Agent.

## Backup

Follow the fence and backup section of the cutover runbook. Take the final
consistent backup after the write fence is verified and before any template,
leadership or archive write. Keep the deployed backend and web/desktop artifacts
with it; a database restore without the matching binaries is not a rollback.

## Migrate

1. Stop related squad default entry points and task dispatch, and freeze
   assignment, contract writes and routing changes. This is the same fence as
   the cutover runbook; pausing only the web UI is not a fence.
2. Deploy the additive schema and backend. Keep
   `MULTICA_EXTERNAL_AGENT_TEMPLATES_ENABLED` **off**. Verify every concurrent
   index completed and read the migration ledger back.
3. Deploy web/desktop and CLI readers/writers that understand leadership roots,
   session fields and the archive preview. No old writer may still resolve the
   global squad leader while this runs.
4. While the switch is off, existing behaviour is unchanged: registration still
   mints one Agent per host session, the list keeps the per-session row, and the
   archive action is the old one. Confirm that in the acceptance workspace.

## Open the switch

Only after step 4:

1. For each squad with an external default leader, a human with squad
   management permission picks a replacement that is a **managed** agent in the
   same workspace. The write is one transaction: lock the squad, the target
   agent and the open tasks; re-check the target is managed, active and callable;
   store the existing leaders as task-level roots before replacing the default.
   Any failed evidence check rolls the whole squad back and the squad stays
   stopped for explicit handoff. No tool ships for this step in slice 3: it is a
   reviewed one-off script run by a human with squad management permission, and
   this slice's rehearsal did not perform a default-leader replacement.
2. Verify the read projection: `GET /api/squads/{id}` has no
   `default_leader_unavailable_reason`, and the squad UI shows a managed leader.
3. Turn `MULTICA_EXTERNAL_AGENT_TEMPLATES_ENABLED` on for one backend and verify
   the switch boundary: a registration request while off returns
   `400 external_agent_templates_disabled` with no partial write; while on, two
   sessions of one host resolve to one template and two registered sessions.
4. Rotate legacy wide tokens: mint a session-scoped token per registered session,
   update every live session, then revoke the old token. A multi-session legacy
   Agent that cannot be updated stays parked.
5. Run the acceptance flow below, then remove the fence.

## Acceptance flow

- Register two sessions under one host; confirm one template, two sessions, two
  session-scoped tokens.
- Each session concurrently creates a squad task. Confirm each issue's
  `leader_agent_id` is the template and `leader_external_session_id` is that
  session, `leadership_root_issue_id` is the issue itself,
  `leadership_revision = 1`, the current delegation return names the same
  session, and the assignment message's `target_session_id` is the same
  session. Neither session may be pinned to the other's address.
- A sibling session commenting on the other's task gets `201` and no dispatch;
  a dispatch mention gets `409`.
- A squad default-leader write with an external Agent returns `400`
  `default_leader_must_be_managed`, with no member or config write.
- The archive preview lists in-flight references; archiving an external Agent
  with a live reference returns `409 agent_has_in_flight_references`; with none,
  it archives and revokes every live credential in the same transaction.

## Rollback boundary

- **Before** the first task-level root, default-leader replacement or
  session-scoped credential write: stop dispatch, turn the switch off, run the
  previous binaries. Additive tables stay; no down migration is required.
- **After** any of those writes: do not return to the global-default resolver and
  do not drop the session or leadership columns. Keep new registration and
  dispatch closed, preserve data and credentials, and roll forward with a fixed
  build. A backup restore loses the new delivery and is not a no-loss rollback.
- Archiving is irreversible for credentials but not for history: restore the
  Agent through the existing restore path; revoked tokens stay revoked, so the
  session must register again or mint a new credential.

## Isolated rehearsal evidence

Rehearsed 2026-11-11 on an isolated database created by
`scripts/init-worktree-env.sh` (`multica_worktree_109`) at the delivery commit:

- `cd server && DATABASE_URL=... go run ./cmd/migrate up` applied
  `655`-`661` ("Done."), including the two pre-migration index hooks.
- With `MULTICA_EXTERNAL_AGENT_TEMPLATES_ENABLED=true`, two `template: true`
  registrations for one host resolved to one template with two registered
  sessions; the server minted one session-scoped credential per session.
- Both session credentials concurrently created a squad task: each issue became
  its own leadership root (`revision = 1`), the template was its leader Agent,
  and each issue's leader session, current delegation return and assignment
  message target were that session and not its sibling's.
- With the switch off, the same registration request returned
  `400 external_agent_templates_disabled` and wrote nothing.
- The rehearsal created only isolated-database rows; no production Agent,
  squad, credential or switch was touched.

## Not covered

- Real host presence/liveness probing (the list shows server-computed
  deliverability, not a host-session online probe).
- Production data, real archives, production configuration or opening the
  switch outside the rehearsal database.
