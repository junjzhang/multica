"use client";

import { useMemo } from "react";
import { Mailbox, Users } from "lucide-react";
import type { Agent, Issue, MailboxMessage } from "@multica/core/types";
import { agentExecutionKind } from "@multica/core/agents/execution-kind";
import { useActorName } from "@multica/core/workspace/hooks";
import { ActorAvatar } from "../../common/actor-avatar";
import { cn } from "@multica/ui/lib/utils";
import { useT, useTimeAgo } from "../../i18n";
import { AgentExecutionStatus, ExternalAgentCard } from "./agent-execution-identity";
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

function deliveryTone(status: MailboxMessage["delivery_status"]): string {
  if (status === "failed") return "text-destructive";
  if (status === "delivered") return "text-foreground";
  return "text-muted-foreground";
}

/**
 * The fixed coordinator, external participants and recent mailbox deliveries.
 * Delivery and processing are rendered as independent facts: a delivered
 * message may still be unread, and only "handled" means the recipient finished
 * with it.
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
  const participants = issue.external_participants ?? [];
  const deliveries = issue.mailbox_deliveries ?? [];
  const coordinator = issue.coordinator ?? null;

  const coordinatorAgent =
    coordinator?.type === "agent"
      ? participants.find((agent) => agent.id === coordinator.id) ?? agentsById.get(coordinator.id) ?? null
      : null;

  if (!coordinator && participants.length === 0 && deliveries.length === 0) return null;

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-1 px-2 text-caption font-medium text-muted-foreground">
        <Users className="size-3" />
        {t(($) => $.detail.external_section_title)}
      </div>

      {coordinator && (
        <div className="space-y-1.5 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.external_coordinator)}
          </div>
          {coordinator.type === "agent" && coordinatorAgent ? (
            agentExecutionKind(coordinatorAgent) === "external" ? (
              <ExternalAgentCard
                agent={coordinatorAgent}
                ownerName={coordinatorAgent.owner_id ? getMemberName(coordinatorAgent.owner_id) : null}
              />
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

      {participants.length > 0 && (
        <div className="space-y-2 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.external_participants)}
          </div>
          {participants.map((agent) => (
            <ExternalAgentCard
              key={agent.id}
              agent={agent}
              ownerName={agent.owner_id ? getMemberName(agent.owner_id) : null}
            />
          ))}
        </div>
      )}

      {deliveries.length > 0 && (
        <div className="space-y-2 pl-2">
          <div className="text-micro font-medium text-muted-foreground">
            {t(($) => $.detail.external_mailbox_deliveries)}
          </div>
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
                  {delivery.source_session && (
                    <div className="mt-1">
                      <ExternalSessionLink session={delivery.source_session} />
                    </div>
                  )}
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
