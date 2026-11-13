"use client";

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { agentSessionsOptions } from "@multica/core/agents/session-bindings";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { useT } from "../../i18n";
import { SessionChoice } from "./session-choice";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

export function unavailableLabel(reason: string | undefined, t: IssuesT): string | null {
  switch (reason) {
    case "agent_archived":
      return t(($) => $.detail.external_binding_agent_archived);
    case "owner_not_member":
      return t(($) => $.detail.external_binding_owner_not_member);
    default:
      return reason ? t(($) => $.detail.external_binding_unavailable) : null;
  }
}

export function AgentSessionChoices({ agentId, selected, onSelect }: {
  agentId: string;
  selected: string;
  onSelect: (id: string) => void;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const sessions = useQuery(agentSessionsOptions(wsId, agentId));
  return (
    <div className="max-h-80 space-y-1.5 overflow-y-auto pr-1">
      {sessions.isPending && (
        <div className="flex items-center gap-1.5 text-micro text-muted-foreground">
          <Spinner aria-hidden className="size-3" />
          {t(($) => $.detail.external_binding_loading_sessions)}
        </div>
      )}
      {sessions.isError && (
        <p className="text-caption text-destructive">
          {t(($) => $.detail.external_binding_sessions_failed)}
        </p>
      )}
      {sessions.data && sessions.data.length === 0 && (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.detail.external_binding_no_sessions)}
        </p>
      )}
      {sessions.data && sessions.data.length > 0 && (
        <div role="radiogroup" aria-label={t(($) => $.detail.external_binding_choose_title)} className="space-y-1.5">
          {sessions.data.map((session) => (
            <SessionChoice
              key={session.id}
              value={session.id}
              title={session.display_name || session.session_id}
              available={session.deliverable}
              reason={unavailableLabel(session.unavailable_reason, t)}
              details={<>
                {t(($) => $.detail.external_session_meta, { provider: session.provider || "external", host: session.host_id })}
                <br />
                {t(($) => $.detail.external_session_id, { id: session.session_id })}
              </>}
              name={`agent-session-${agentId}`}
              selected={session.id === selected}
              onSelect={() => onSelect(session.id)}
            />
          ))}
        </div>
      )}
    </div>
  );
}
