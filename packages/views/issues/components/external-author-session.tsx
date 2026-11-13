"use client";

import type { Agent } from "@multica/core/types";
import { isExternalAgent } from "@multica/core/agents/execution-kind";
import { useT } from "../../i18n";
import { ExternalSessionLink } from "./external-session-link";

/**
 * Resolve an agent author's external session from the workspace directory
 * already loaded by the issue. Managed authors render nothing here, so they
 * keep their normal avatar/name (and, where applicable, the managed
 * missing-runtime path elsewhere).
 */
export function ExternalAuthorSession({
  agentId,
  agents,
}: {
  agentId: string;
  agents?: Agent[];
}) {
  const { t } = useT("issues");
  const agent = agents?.find((candidate) => candidate.id === agentId);
  if (!agent || !isExternalAgent(agent)) return null;

  if (!agent.external_session) {
    return (
      <span className="shrink-0 text-micro text-muted-foreground">
        {t(($) => $.detail.external_session_unavailable)}
      </span>
    );
  }

  return <ExternalSessionLink session={agent.external_session} className="shrink-0" />;
}
