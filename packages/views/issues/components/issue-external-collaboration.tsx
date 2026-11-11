"use client";

import { useMemo } from "react";
import { Mailbox, Users } from "lucide-react";
import type { Agent, Issue, IssueSessionBinding, MailboxMessage } from "@multica/core/types";
import { agentExecutionKind } from "@multica/core/agents/execution-kind";
import { useActorName } from "@multica/core/workspace/hooks";
import { ActorAvatar } from "../../common/actor-avatar";
import { cn } from "@multica/ui/lib/utils";
import { useT, useTimeAgo } from "../../i18n";
import { AgentExecutionStatus, ExternalAgentCard } from "./agent-execution-identity";
import { AgentIssueSessionBinding } from "./agent-issue-session-binding";
import { ExternalSessionLink } from "./external-session-link";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

function deliveryLabel(status: MailboxMessage["delivery_status"], t: IssuesT): string {
  switch (status) {
    case "pending":
      return t(($) => $.detail.mailbox_delivery_pending);
    case "leased":
      return t(($) => $.detail.mailbox_delivery_leased);
    case "delivered":
      return t(($) => $.detail.mailbox_delivery_delivered);
    case "failed":
      return t(($) => $.detail.mailbox_delivery_failed);
    default:
      return t(($) => $.detail.mailbox_status_unknown);
  }
}

function processingLabel(status: MailboxMessage["processing_status"], t: IssuesT): string {
  switch (status) {
    case "unread":
      return t(($) => $.detail.mailbox_processing_unread);
    case "read":
      return t(($) => $.detail.mailbox_processing_read);
    case "handled":
      return t(($) => $.detail.mailbox_processing_handled);
    default:
      return t(($) => $.detail.mailbox_status_unknown);
  }
}

/**
 * Known producer kinds get a purpose label. Anything newer keeps a stable
 * "other notification" label instead of disappearing or being guessed.
 */
function deliveryKindLabel(kind: string | undefined, t: IssuesT): string {
  switch (kind) {
    case "comment_mention":
      return t(($) => $.detail.mailbox_kind_comment);
    case "issue_wakeup":
      return t(($) => $.detail.mailbox_kind_wakeup);
    default:
      return t(($) => $.detail.mailbox_kind_unrecognized);
  }
}

function deliveryTone(status: MailboxMessage["delivery_status"]): string {
  if (status === "failed") return "text-destructive";
  if (status === "delivered") return "text-foreground";
  return "text-muted-foreground";
}

/**
 * The fixed coordinator, external participants, their issue-specific session
 * targets and recent mailbox deliveries. Delivery and processing are rendered
 * as independent facts: a delivered message may still be unread, and only
 * "handled" means the recipient finished with it. An unknown outcome is
 * explicitly unconfirmed and this view never offers a resend.
 */
export function IssueExternalCollaboration({
  issue,
  agents,
}: {
  issue: Issue;
  agents: Agent[];
}) {
  const { t } = useT("issues");
  const timeAgo = useTimeAgo();
  const { getActorName, getMemberName } = useActorName();

  const agentsById = useMemo(() => new Map(agents.map((agent) => [agent.id, agent])), [agents]);
  const participants = useMemo(() => issue.external_participants ?? [], [issue.external_participants]);
  const bindings = useMemo(() => issue.external_session_bindings ?? [], [issue.external_session_bindings]);
  const bindingsKnown = issue.external_session_bindings !== undefined;
  const deliveries = issue.mailbox_deliveries ?? [];
  const coordinator = issue.coordinator ?? null;
  const coordinatorAgentID = coordinator?.type === "agent" ? coordinator.id : null;

  const bindingsByAgent = useMemo(
    () => new Map(bindings.map((binding) => [binding.agent_id, binding])),
    [bindings],
  );

  // `undefined` means the projection failed or is absent; `[]` means the issue
  // authoritatively has no bindings. Do not present a read failure as "none".
  const bindingFor = (agentId: string): IssueSessionBinding | null | undefined =>
    bindingsKnown ? (bindingsByAgent.get(agentId) ?? null) : undefined;

  // A binding is itself evidence of participation, so an agent with a target
  // is shown even when it has not commented on the issue yet.
  const externalAgents = useMemo(() => {
    const seen = new Set<string>();
    const out: Agent[] = [];
    for (const agent of participants) {
      if (agent.id === coordinatorAgentID || seen.has(agent.id)) continue;
      seen.add(agent.id);
      out.push(agent);
    }
    for (const binding of bindings) {
      if (binding.agent_id === coordinatorAgentID || seen.has(binding.agent_id)) continue;
      const agent = agentsById.get(binding.agent_id);
      if (!agent) continue;
      seen.add(binding.agent_id);
      out.push(agent);
    }
    return out;
  }, [participants, bindings, agentsById, coordinatorAgentID]);

  const coordinatorAgent =
    coordinator?.type === "agent"
      ? participants.find((agent) => agent.id === coordinator.id) ?? agentsById.get(coordinator.id) ?? null
      : null;

  // The task-chain leader is the responsible subject for this issue. It is
  // read from the issue's own leadership pointer, never from the squad's
  // global default, so a later default change cannot relabel history.
  const leaderAgent = issue.leader_agent_id
    ? participants.find((agent) => agent.id === issue.leader_agent_id) ??
      agentsById.get(issue.leader_agent_id) ??
      null
    : null;
  const leaderBinding = leaderAgent ? bindingFor(leaderAgent.id) : undefined;
  const leaderSession =
    leaderBinding?.session && leaderBinding.session.id === issue.leader_external_session_id
      ? leaderBinding.session
      : null;
  const leaderIsExternal = leaderAgent !== null && agentExecutionKind(leaderAgent) === "external";

  if (
    !coordinator &&
    externalAgents.length === 0 &&
    deliveries.length === 0 &&
    !issue.leadership_root_issue_id
  ) {
    return null;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-1 px-2 text-caption font-medium text-muted-foreground">
        <Users className="size-3" />
        {t(($) => $.detail.external_section_title)}
      </div>

      {issue.leadership_root_issue_id && (
        <div className="space-y-1.5 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.task_leader)}
          </div>
          {leaderAgent ? (
            leaderIsExternal ? (
              <ExternalAgentCard
                agent={leaderAgent}
                ownerName={leaderAgent.owner_id ? getMemberName(leaderAgent.owner_id) : null}
                showSession={false}
              />
            ) : (
              <div className="flex min-w-0 items-center gap-2">
                <ActorAvatar actorType="agent" actorId={leaderAgent.id} size="sm" enableHoverCard />
                <span className="truncate text-caption font-medium">{leaderAgent.name}</span>
              </div>
            )
          ) : (
            <p className="text-micro text-destructive">
              {t(($) => $.detail.task_leader_unavailable)}
            </p>
          )}
          {leaderAgent && leaderIsExternal && (
            leaderSession ? (
              <div className="pl-8">
                <ExternalSessionLink
                  session={leaderSession}
                  templateName={leaderAgent.external_template?.name}
                />
              </div>
            ) : (
              <p className="pl-8 text-micro text-muted-foreground">
                {t(($) => $.detail.task_leader_session_unknown)}
              </p>
            )
          )}
        </div>
      )}

      {coordinator && (
        <div className="space-y-1.5 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.external_coordinator)}
          </div>
          {coordinator.type === "agent" && coordinatorAgent ? (
            agentExecutionKind(coordinatorAgent) === "external" ? (
              <div className="space-y-1">
                <ExternalAgentCard
                  agent={coordinatorAgent}
                  ownerName={coordinatorAgent.owner_id ? getMemberName(coordinatorAgent.owner_id) : null}
                  showSession={false}
                />
                <div className="pl-8">
                  <AgentIssueSessionBinding
                    agent={coordinatorAgent}
                    issueId={issue.id}
                    binding={bindingFor(coordinatorAgent.id)}
                  />
                </div>
              </div>
            ) : (
              <div className="min-w-0">
                <div className="flex min-w-0 items-center gap-2">
                  <ActorAvatar actorType="agent" actorId={coordinatorAgent.id} size="sm" enableHoverCard />
                  <span className="truncate text-caption font-medium">{coordinatorAgent.name}</span>
                </div>
                <AgentExecutionStatus agent={coordinatorAgent} detailed={false} className="mt-1" />
              </div>
            )
          ) : (
            <div className="flex min-w-0 items-center gap-2">
              <ActorAvatar actorType="member" actorId={coordinator.id} size="sm" enableHoverCard />
              <span className="truncate text-caption font-medium">
                {getActorName(coordinator.type, coordinator.id)}
              </span>
            </div>
          )}
        </div>
      )}

      {externalAgents.length > 0 && (
        <div className="space-y-2 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.external_participants)}
          </div>
          {externalAgents.map((agent) => {
            const binding = bindingFor(agent.id);
            return (
              <div key={agent.id} className="space-y-1">
                <ExternalAgentCard
                  agent={agent}
                  ownerName={agent.owner_id ? getMemberName(agent.owner_id) : null}
                  showSession={false}
                />
                <div className="pl-8">
                  <AgentIssueSessionBinding agent={agent} issueId={issue.id} binding={binding} />
                </div>
              </div>
            );
          })}
        </div>
      )}

      {issue.leader_external_session_id && externalAgents.length > 0 && (
        <p className="pl-2 text-micro text-muted-foreground">
          {t(($) => $.detail.sibling_session_note)}
        </p>
      )}

      {deliveries.length > 0 && (
        <div className="space-y-2 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.external_mailbox_deliveries)}
          </div>
          <p className="text-micro text-muted-foreground">
            {t(($) => $.detail.mailbox_delivery_note)}
          </p>
          <div className="flex flex-col gap-1.5">
            {deliveries.map((delivery) => {
              const recipient = agentsById.get(delivery.recipient_agent_id);
              return (
                <div
                  key={delivery.id}
                  className="min-w-0 rounded-md border border-border/70 p-2 text-micro"
                >
                  <div className="flex min-w-0 items-center gap-1.5">
                    <Mailbox className="size-3 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 truncate font-medium text-caption">
                      {recipient?.name ?? delivery.recipient_agent_id}
                    </span>
                    <span
                      className="shrink-0 rounded-sm bg-muted px-1 py-0.5 text-micro text-muted-foreground"
                      title={delivery.kind || undefined}
                    >
                      {deliveryKindLabel(delivery.kind, t)}
                    </span>
                    <span className="ml-auto shrink-0 text-muted-foreground">
                      {timeAgo(delivery.created_at)}
                    </span>
                  </div>
                  <div className="mt-1 flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
                    <span className={cn("font-medium", deliveryTone(delivery.delivery_status))}>
                      {deliveryLabel(delivery.delivery_status, t)}
                    </span>
                    <span aria-hidden className="text-muted-foreground">·</span>
                    <span className="font-medium text-muted-foreground">
                      {processingLabel(delivery.processing_status, t)}
                    </span>
                  </div>
                  {delivery.delivery_status === "unknown" && (
                    <p className="mt-1 text-muted-foreground">
                      {t(($) => $.detail.mailbox_unknown_hint)}
                    </p>
                  )}

                  <div className="mt-1 space-y-1">
                    <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                      <span className="text-muted-foreground">
                        {t(($) => $.detail.mailbox_target_session)}
                      </span>
                      {delivery.target_session ? (
                        <ExternalSessionLink session={delivery.target_session} />
                      ) : (
                        <span className="text-muted-foreground">
                          {t(($) => $.detail.mailbox_target_missing)}
                        </span>
                      )}
                    </div>
                    {delivery.source_session && (
                      <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                        <span className="text-muted-foreground">
                          {t(($) => $.detail.mailbox_source_session)}
                        </span>
                        <ExternalSessionLink session={delivery.source_session} />
                      </div>
                    )}
                    {delivery.comment_id && (
                      <a
                        href={`#comment-${delivery.comment_id}`}
                        className="inline-block text-muted-foreground underline underline-offset-2 hover:text-foreground"
                      >
                        {t(($) => $.detail.mailbox_comment_reference)}
                      </a>
                    )}
                  </div>

                  {delivery.delivered_at && (
                    <div className="mt-1 text-muted-foreground">
                      {t(($) => $.detail.mailbox_delivered_at, { time: timeAgo(delivery.delivered_at) })}
                    </div>
                  )}
                  {delivery.last_error && (
                    <div className="mt-1 break-words text-destructive">{delivery.last_error}</div>
                  )}
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
