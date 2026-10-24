"use client";

import type { Agent } from "@multica/core/types";
import {
  agentExecutionKind,
  agentNeedsRuntime,
  isExternalAgent,
} from "@multica/core/agents/execution-kind";
import { ActorAvatar } from "../../common/actor-avatar";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { ExternalSessionLink } from "./external-session-link";

/**
 * The execution half of an agent identity. External agents show their concrete
 * host session; managed agents keep their existing missing-runtime repair
 * signal. An unknown execution_kind is neither: it renders as unsupported so
 * the UI never guesses an execution boundary.
 */
export function AgentExecutionStatus({
  agent,
  detailed = true,
  className,
}: {
  agent: Agent;
  detailed?: boolean;
  className?: string;
}) {
  const { t } = useT("issues");
  const kind = agentExecutionKind(agent);

  if (kind === "external") {
    if (!agent.external_session) {
      return (
        <span className={cn("text-micro text-muted-foreground", className)}>
          {t(($) => $.detail.external_session_unavailable)}
        </span>
      );
    }
    return (
      <ExternalSessionLink
        session={agent.external_session}
        detailed={detailed}
        className={className}
      />
    );
  }

  if (kind === "unknown") {
    return (
      <span className={cn("text-micro text-destructive", className)}>
        {t(($) => $.detail.external_execution_unknown)}
      </span>
    );
  }

  if (agentNeedsRuntime(agent)) {
    return (
      <span className={cn("text-micro text-warning", className)}>
        {t(($) => $.detail.agent_needs_runtime)}
      </span>
    );
  }

  return null;
}

/**
 * One external participant/coordinator card: the same agent identity the
 * comment author uses, plus its fixed session. Managed agents are not rendered
 * here because this card represents the external-execution surface.
 */
export function ExternalAgentCard({
  agent,
  ownerName,
  className,
}: {
  agent: Agent;
  ownerName?: string | null;
  className?: string;
}) {
  const { t } = useT("issues");
  const kind = agentExecutionKind(agent);

  return (
    <div className={cn("flex min-w-0 items-start gap-2", className)}>
      <ActorAvatar
        actorType="agent"
        actorId={agent.id}
        name={agent.name}
        avatarUrl={agent.avatar_url}
        profileRequiresDirectoryEntry
        size="sm"
        enableHoverCard
      />
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 flex-wrap items-baseline gap-x-1.5">
          <span className="min-w-0 truncate text-caption font-medium">{agent.name}</span>
          {kind === "external" && (
            <span className="shrink-0 text-micro text-muted-foreground">
              {t(($) => $.detail.external_agent_label)}
            </span>
          )}
        </div>
        <AgentExecutionStatus agent={agent} detailed className="mt-1" />
        {ownerName && (
          <div className="mt-0.5 truncate text-micro text-muted-foreground">
            {t(($) => $.detail.external_owned_by, { name: ownerName })}
          </div>
        )}
      </div>
    </div>
  );
}

export { isExternalAgent };
