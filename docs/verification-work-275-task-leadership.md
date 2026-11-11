# WORK-275 task leadership and session boundaries

Implementation follows design PR #18 at `533958f86c1b2fc92db44077b5b5bde0a9e1a360`, on top of slice 1 at `3d2f38f8a06d19e217698735512181906be3f1d1`. This slice changes backend/CLI contracts and additive response parsing. It does not enable template registration or deploy migrations.

## State and entry points

`issueleader.ResolveIssueLeader` reads the task's one-hop leadership root. An applicable root is authoritative; invalid membership, a revoked session, or an archived leader makes it unavailable. A new entry without an applicable root uses a managed default. External initiation instead claims the initiating credential's qualified member session inside the assignment transaction. Merely setting `parent_issue_id` does not join roots; create accepts an explicit `leadership_root_issue_id`.

`POST /api/issues/{id}/leadership/claim` uses `expected_revision` and `expected_issue_revision`; concurrent claims have one winner. Human owner/admin callers use `POST /api/issues/{id}/task-leader/handoff` to change the task leader, including from a subtask. The request includes `client_request_id`, `previous_agent_id`, `previous_session_id`, `agent_id`, `session_id`, `expected_revision`, `expected_issue_revision`, and `reason`; both revisions refer to the leadership root. Managed targets omit session IDs. For a directly assigned external executor, `POST /api/issues/{id}/executor/handoff` compares the old binding and issue revision and changes the execution binding without inventing a squad root. Result takeover cannot change that execution binding. The CLI maps these operations to `multica issue leadership claim|handoff <issue> --request-file ./request.json` and `multica issue executor handoff <issue> --request-file ./request.json`.

The deprecated `/leadership/handoff` endpoint retains its original assignee-based dispatch during the upgrade window, including its existing behavior for subtask IDs. It sends a `Deprecation` response header. Remove it only after all repository callers have migrated and at least one release cycle has passed. The shared web and desktop dialog and CLI use the explicit endpoints; mobile and repository plugins have no direct handoff caller. Existing deployed clients may continue using the old endpoint during that window.

Handoff preserves original authors, accepted return versions, unknown receipts, running children, and local wait addresses. It withdraws old pending execution deliveries, fences queued/start claims by leadership revision, and makes affected contracts stale. A human can receive a specific historical result separately through the existing result-takeover endpoint. Its `target.agent_id` now permits a different external template; a zero UUID in `expected_current_return_id` means the new target has no current return. The new relation and notification record their source return and do not rewrite the original dispatcher.

## Confirmed design clarification

The requester approved this boundary in WORK-275 comment `01a1c88a-41f6-7206-8ba8-7596fc1f3fa4`: changing a root's assigned squad returns 409 when explicit child references still exist, and identifies those children. Without references, assignment creates the target squad's next leadership revision in its transaction. Same-squad leader changes use handoff. Root history and a detach protocol are out of scope.

## Consumer inventory

Search scope: tracked repository source for `leader_id` and `LeaderID`, excluding generated SQL copies and tests when classifying runtime callers. Historical migrations, documentation, client DTOs, and configuration UI are classified separately below; none are dispatch authority for an adopted task.

| Consumer | Classification and implementation |
| --- | --- |
| `internal/service/issue_external_assignment.go`, `issue.go` | Assignment resolves/establishes the root in the business transaction; managed queue and external Mailbox use the resolved agent/session. |
| `internal/service/issue_trigger.go` | Trigger preview uses the same issue leader, including readiness and permissions. |
| `internal/handler/comment.go`, `mailbox.go`, `comment_obligation.go` | `@squad`, durable native obligations, and Mailbox producers resolve task leadership; guest squads use their own default without changing the owning root. Sibling comments suppress all execution triggers; executable mentions return 409 before a business write. |
| `internal/service/issue_wakeup.go`, `issue_wakeup_system.go` | Explicit subscriptions freeze their session and leadership revision. Dispatch disables obsolete subscriptions rather than retargeting them. System child/stage wakeups resolve the current root, including external Mailbox delivery. |
| `pkg/db/queries/workspace_wakeup.sql` | System-wakeup summaries use `issue_leader_id`, matching dispatch. |
| `internal/service/task.go`, `internal/handler/task_lifecycle.go` | Current-assignment reruns use the task leader. Historical returns remain snapshots. |
| `internal/handler/issue_workflow_entry.go` | Workflow handoff attribution resolves the issue leader rather than the squad default. |
| `internal/handler/daemon.go`, `squad.go`, `squad_external_evaluation.go` | Claim briefing and leader-activity authorization use the task leader. Quick-create has no issue yet and uses a validated managed default. |
| `internal/squadbriefing/briefing.go` | `BuildForIssue` names the actual leader, root, revision, and session; roster exclusion uses the actual leader. `Build` is used only before an issue exists. The default is labeled separately. |
| `internal/handler/issue_contract.go`, `issue_contract_authority.go`, migration 659 | Adopted contract basis includes executor, representative, requester/acceptor session snapshots and root revisions. Delivery/decision authors include their actual session. Legacy unadopted contracts retain the original basis shape and can continue only through their exact binding or a proven sole registered session; an explicit revision adopts the new session basis. |
| `internal/service/autopilot.go` | Configuration resolves a managed default. Issue creation binds that default transactionally and rechecks it against the selected agent. Legacy external defaults produce a visible skipped outcome without being replaced or replayed later. |
| `internal/handler/autopilot.go`, `issue.go` quick-create, `source_context.go` | New-entry validation checks default type, workspace, archive state, and existing permissions. External task initiation validates the actual initiating member and then the resolved task executor. |
| `internal/handler/quick_action.go` | Execution/render uses task leadership; configuration catalog previews display the default. |
| `pkg/db/queries/issue.sql`, `internal/handler/issue.go`, `issue_table_query.go` | Ownership/involvement projections join `issue_leader_id` rather than a mutable squad default. |
| `internal/handler/squad.go` create/update | Pure default configuration writes. Managed type and atomic leader membership are enforced. |
| `internal/handler/squad.go` archive, `pkg/db/queries/squad.sql` transfer | Archive serializes with assignment, blocks unresolved active leadership chains, and transfers only legacy issues without roots. Automation configuration is handled separately from existing issue roots. |
| `pkg/db/queries/squad.sql` member ordering, `squad.go` DTO/member removal | Pure default configuration and display. Changing these values does not update existing leadership roots. |
| `pkg/db/queries/autopilot.sql`, runtime teardown in `agent.sql` | Default configuration visibility and configuration pause rules; they do not select an existing task executor. |
| `internal/issueleader/leader.go`, migration 659 `issue_leader_id` | The two explicit default-resolution boundaries, used only when the requested squad has no applicable root. |
| `cmd/multica/cmd_squad.go`, frontend/mobile squad DTOs/selectors | Default configuration input/display. Existing clients are protected by server-side type checks. UI migration is a later slice. |
| Built-in skill documentation, older design documents, migrations 084/096/237/654 and the legacy basis helper | Documentation or immutable legacy contracts. The legacy basis helper is selected only for issues that have not adopted root/session semantics. |
| Generated SQL, logs mentioning `leader_id` | Mechanical copies or diagnostics, not independent routing decisions. |

## Verification

- Core response-schema regression: 5 tests passed.
- Root frontend `pnpm typecheck`: 10 workspace tasks passed; mobile excluded by the repository command.
- Targeted Go/DB regressions cover A/B independent roots, contract session authors, default rotation, sibling comment/dispatch, claim CAS, handoff idempotency/staleness, cross-template result receipt, default type rejection, and the three slice-1 revocation regressions.
- Migration up/down was exercised only on this worktree's isolated database, created by `make up`; no production data or configuration was changed.

The delivery comment and PR record the final revision, completed checks, review conclusions, and any remaining validation gaps.

## Review regression and execution fences

The first independent review reproduced root-row `NOWAIT` contention on ordinary title/comment writes. Resolution now reads committed state without a row lock. Enqueue/start triggers and the delivery-claim transaction take waiting locks bounded by three seconds; the leadership revision is checked under that lock. Contract writes pin their referenced roots and recheck their authenticated session. A handoff uses nonblocking queue-row cancellation so it cannot deadlock a claim that already owns the task row.

`TestTaskLeadershipOrdinaryWriteDoesNotBreakDispatch` failed before this correction and passed afterward for managed claim, managed start, external Mailbox claim, and `@squad` preview. `TestTaskLeadershipHandoffConflictsWithAClaimedQueueRow` verifies rollback and safe retry. Replacement runs carry the existing assignment intent's frozen upstream returns. Same-template handoff advances only the current dispatcher version; old run obligations keep their original version, and the new session can delegate afterward.

## Accepted host limitation

The local host runs Linux `6.6.119-49.20.tl4.x86_64`. Four pre-existing Cursor process-group tests require the pidfd group-signalling facility used by `pkg/agent/cursor_background_process_linux.go`. The same failure (`retain Cursor shell process group: invalid argument`) was reproduced from archived baseline `3d2f38f8` with:

```sh
go test -race ./pkg/agent -run '^TestCaptureCursorBackgroundLateDescendant$' -count=1 -v
```

A signal-0 syscall probe on a test-created process group succeeded with flags 0 and returned `EINVAL` with flags 4. WORK-275 requester comment `01a1c8ac-44e7-7cd0-9bcc-62c8e5f2df06` accepted this as a baseline host limitation; the Cursor implementation and default tests were not weakened. The remaining agent tests passed with the repository's executable guard and `-parallel 2`, excluding exactly:

- `TestCaptureCursorBackgroundLateDescendant`
- `TestCaptureCursorBackgroundProcessReapsLeaderExitedGroup`
- `TestCaptureCursorBackgroundSurvivesRootExitDuringCapture`
- `TestCursorBackgroundLifecycle`

Real-agent smoke tests, production migration/deployment, mobile verification, and platform-specific macOS/Windows checks are outside this slice's local run. GitHub PR ingestion is unavailable in this workspace; the requester explicitly accepted the PR URL and revision SHA as delivery evidence in comment `01a1c8a0-486b-7b96-8a43-f9c5ba5158b6` and asked not to retry linkage.
