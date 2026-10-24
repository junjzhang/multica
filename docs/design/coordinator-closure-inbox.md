# Coordinator closure and human Inbox

The coordinator owns closure of delegated work; the human does not track every
implementer. Routine subagent activity stays observable in the GUI, while the
human Inbox receives only events that need a human. This is prompt policy and
Inbox-creation policy: the Issue API's access model, wakeup routing, and shared
workspace events are unchanged.

## Roles and grants

- **Implementer.** Completes the assigned work and ends the turn at `in_review`.
  It never records acceptance of its own delivery and never closes the issue.
- **Authorized coordinator.** Only a coordinator the task explicitly authorizes
  verifies the review and acceptance evidence at the exact revision it names,
  records acceptance, and closes the accepted sub-issue. That closure is an
  ordinary status mutation: there is no new status, approval service, token
  privilege, or automatic self-acceptance.
- **Parent delivery.** Accepting a sub-issue does not deliver its parent. The
  parent's own delivery and its acceptance remain separately tracked, and a
  coordinator reports each delivery rather than inferring one from the other.
- **Merge authorization.** A separate human-granted policy
  (`merge_authorization.allowed`, disallowed by default). Acceptance is not
  merge authorization, and authorization does not replace review or current-head
  CI.
- **Human.** Owns final product acceptance and the decisions an agent may not
  make. The coordinator hands a result back by its result comment; it does not
  mark final acceptance on the human's behalf.

Delivery evidence is the review and validation actually run, plus the revision
they apply to. Observability in the GUI is not acceptance.

## Handoff

A next-action handoff is an explicit `@` mention of the registered recipient.
A managed agent gets a dispatched run; an external session gets one mailbox
request through the existing delivery path. A coordinator with no registered
identity cannot be woken by the platform, so a handoff must not promise it.

The sub-issue completion wakeup keeps its existing semantics: it targets the
parent's assignee by stage. `in_review` is not `done`, so a delivered child
closes no stage barrier and wakes no parent. A coordinator who is not that
assignee is reached by the explicit mention in the delivery comment.

## Human Inbox

Inbox creation lives in `server/cmd/server/notification_listeners.go`; the
workspace event stream in `listeners.go` is independent and keeps publishing for
the GUI. This change touches Inbox creation only.

Routine child activity is quiet for implicit subscriptions:

- child status progress (`in_progress`, `in_review`, `done`), ordinary
  comments, and priority or due-date updates;
- implicit creator, commenter, and delegated subscriptions, including
  subscriptions inherited from the parent.

These remain effective:

- a human's direct manual subscription to that exact child
  (`issue_subscriber.reason=manual`) and an explicit human assignee on it; a
  manual subscription to the parent is not opt-in to every child;
- explicit human mentions and assignments;
- `blocked` and unknown custom handoff statuses, including possible approval
  requests;
- final task failures and exhausted recovery, root issue delivery, and existing
  parent summaries;
- membership, mute preferences, and existing deduplication.

A task failure with `retry_pending=true` still reaches GUI and event consumers,
but creates no human failure Inbox row. The human failure notification waits
until recovery has actually exhausted; existing terminal-failure paths stay.

Nothing is deleted or hidden: subscriptions, tasks, comments, runs, and existing
Inbox history remain, child issues stay visible in the GUI, and shared workspace
events are not suppressed. Root comments keep their existing behavior: there is
no reliable final-delivery discriminator, and this change does not invent one.

## Limits

- The runtime brief is rendered by the daemon and built-in skills are served by
  the backend, so an installed daemon keeps its old status policy until it is
  upgraded. A backend release alone cannot rewrite an already-installed brief.
- Acceptance by a coordinator is a policy decision, not a server-enforced
  approval step. The platform does not distinguish an authorized coordinator
  from any other actor that can write status.
- The coordinator role is carried by task instructions and explicit mentions,
  not by a new field or state machine.

## Verification

- Render-level tests pin the runtime brief and the `multica-platform` issue
  reference: implementer, authorized coordinator, final acceptance and merge
  stay distinct; `in_review` stays non-terminal; handoffs name a registered
  recipient; the retired unconditional human `done` ceiling does not return.
- DB/EventBus notification tests prove routine child events create no automatic
  human Inbox row, while direct manual child subscription, human assignee,
  explicit human mention, `blocked`, custom handoff, final failure, and root
  behavior remain.
- `retry_pending` failures stay observable to GUI/event consumers without a
  human Inbox row, and normal issue/comment/task event delivery and
  working-agent projections are unchanged.
- Focused package tests run locally; full integration, acceptance, PR, and
  deployment belong to the parent coordinator.
