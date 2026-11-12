"use client";

import { useState } from "react";
import { ChevronDown } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import type { Agent } from "@multica/core/types";
import { useWorkspaceId } from "@multica/core/hooks";
import { agentSessionsPageOptions } from "@multica/core/agents/session-bindings";
import { Button } from "@multica/ui/components/ui/button";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@multica/ui/components/ui/popover";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

const PAGE_SIZE = 5;

type AgentsT = ReturnType<typeof useT<"agents">>["t"];

/**
 * Availability, not liveness: the server knows whether a registered session
 * may receive new work (revoked, owner gone, agent archived). It carries no
 * host-session probe yet, so an available session is reported as deliverable
 * instead of claiming it is "online".
 */
export function sessionAvailabilityLabel(
  deliverable: boolean,
  unavailableReason: string | undefined,
  t: AgentsT,
): string {
  if (deliverable) return t(($) => $.row.session_deliverable);
  switch (unavailableReason) {
    case "session_revoked":
      return t(($) => $.row.session_revoked);
    case "agent_archived":
      return t(($) => $.row.session_agent_archived);
    case "owner_not_member":
      return t(($) => $.row.session_owner_missing);
    default:
      return t(($) => $.row.session_status_unknown);
  }
}

/**
 * The template's registered host sessions, paged from the server. A legacy
 * per-session external agent keeps its single-session rendering and never
 * mounts this control, so the template switch staying off keeps the old list
 * behaviour.
 */
export function AgentSessionsPopover({ agent }: { agent: Agent }) {
  const { t } = useT("agents");
  const wsId = useWorkspaceId();
  const [limit, setLimit] = useState(PAGE_SIZE);
  const [open, setOpen] = useState(false);
  const { data, isPending, isError } = useQuery({
    ...agentSessionsPageOptions(wsId, agent.id, limit),
    enabled: open && !!wsId,
  });

  const sessions = data?.sessions ?? [];
  const total = data?.total ?? 0;
  const canShowMore = !isPending && sessions.length < total;

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button
            type="button"
            variant="ghost"
            size="xs"
            className="shrink-0 text-muted-foreground"
            aria-label={t(($) => $.row.sessions_button)}
          />
        }
      >
        <ChevronDown className={cn("size-3 transition-transform", open && "rotate-180")} />
        {t(($) => $.row.sessions_button)}
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 p-0">
        <div className="border-b px-3 py-2.5">
          <p className="text-caption font-medium">{t(($) => $.row.sessions_title)}</p>
          {agent.external_template && (
            <p className="mt-0.5 truncate text-micro text-muted-foreground">
              {agent.external_template.name}
            </p>
          )}
        </div>
        <div className="max-h-72 overflow-y-auto px-3 py-2">
          {isError ? (
            <p role="alert" className="text-micro text-destructive">
              {t(($) => $.row.sessions_error)}
            </p>
          ) : isPending ? (
            <p className="text-micro text-muted-foreground">{t(($) => $.row.sessions_loading)}</p>
          ) : sessions.length === 0 ? (
            <p className="text-micro text-muted-foreground">{t(($) => $.row.sessions_empty)}</p>
          ) : (
            <ul className="space-y-2">
              {sessions.map((session) => {
                const title = session.display_name || session.session_id;
                const status = sessionAvailabilityLabel(
                  session.deliverable,
                  session.unavailable_reason,
                  t,
                );
                return (
                  <li key={session.id} className="min-w-0">
                    <div className="flex min-w-0 items-center gap-1.5">
                      <span
                        className={cn(
                          "size-1.5 shrink-0 rounded-full",
                          session.deliverable ? "bg-emerald-500" : "bg-muted-foreground",
                        )}
                        aria-hidden
                      />
                      <span className="min-w-0 truncate text-caption font-medium" title={title}>
                        {title}
                      </span>
                    </div>
                    <p className="truncate text-micro text-muted-foreground">
                      {session.provider}
                      {" · "}
                      {session.host_id}
                      {" · "}
                      {status}
                    </p>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
        <div className="flex items-center justify-between gap-2 border-t px-3 py-2">
          <span className="text-micro text-muted-foreground">
            {t(($) => $.row.sessions_showing, { shown: sessions.length, total })}
          </span>
          {canShowMore && (
            <Button
              type="button"
              variant="ghost"
              size="xs"
              onClick={() => setLimit((current) => current + PAGE_SIZE)}
            >
              {t(($) => $.row.sessions_more)}
            </Button>
          )}
        </div>
      </PopoverContent>
    </Popover>
  );
}
