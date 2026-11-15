"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link2 } from "@multica/ui/icons";
import type { Agent, IssueSessionBinding } from "@multica/core/types";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAgentPermissions } from "@multica/core/permissions";
import {
  agentSessionsOptions,
  useBindAgentSession,
} from "@multica/core/agents/session-bindings";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@multica/ui/components/ui/dialog";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { useT } from "../../i18n";
import { ExternalSessionLink } from "./external-session-link";
import { SessionChoice } from "./session-choice";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

function unavailableLabel(reason: string | undefined, t: IssuesT): string | null {
  switch (reason) {
    case "agent_archived":
      return t(($) => $.detail.external_binding_agent_archived);
    case "owner_not_member":
      return t(($) => $.detail.external_binding_owner_not_member);
    default:
      return reason ? t(($) => $.detail.external_binding_unavailable) : null;
  }
}

function SessionPicker({
  agent,
  issueId,
  currentSessionId,
  onSaved,
}: {
  agent: Agent;
  issueId: string;
  currentSessionId: string | null;
  onSaved: () => void;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const [selected, setSelected] = useState(currentSessionId ?? "");
  const sessions = useQuery(agentSessionsOptions(wsId, agent.id));
  const bind = useBindAgentSession(wsId);
  const changed = selected !== "" && selected !== currentSessionId;

  const save = async () => {
    if (!changed || bind.isPending) return;
    try {
      await bind.mutateAsync({ agentId: agent.id, issueId, sessionId: selected });
      onSaved();
    } catch {
      // The mutation state renders the error; keep the dialog and the
      // previously displayed binding untouched.
    }
  };

  return (
    <>
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
                name={`agent-session-${agent.id}`}
                selected={session.id === selected}
                onSelect={() => setSelected(session.id)}
              />
            ))}
          </div>
        )}
      </div>

      {bind.isError && (
        <p role="alert" className="mt-2 text-caption text-destructive">
          {t(($) => $.detail.external_binding_save_failed)}
        </p>
      )}

      <DialogFooter>
        <DialogClose render={<Button variant="outline" />}>
          {t(($) => $.detail.external_binding_cancel)}
        </DialogClose>
        <Button
          type="button"
          onClick={save}
          disabled={!changed || bind.isPending}
          aria-busy={bind.isPending}
        >
          {bind.isPending && <Spinner aria-hidden className="size-3.5" />}
          {t(($) => $.detail.external_binding_save)}
        </Button>
      </DialogFooter>
    </>
  );
}

/**
 * The issue-specific session target for one external agent. It is deliberately
 * separate from the agent's detail session: saving here changes only where
 * future notifications for this issue go, and no existing delivery is moved.
 */
export function AgentIssueSessionBinding({
  agent,
  issueId,
  binding,
}: {
  agent: Agent;
  issueId: string;
  /** `undefined` means the issue projection could not be read. */
  binding: IssueSessionBinding | null | undefined;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { canEdit, isLoading } = useAgentPermissions(agent, wsId);
  const [open, setOpen] = useState(false);
  const reason = unavailableLabel(binding?.unavailable_reason, t);

  return (
    <div className="mt-1 space-y-1">
      <div className="text-micro font-medium text-muted-foreground">
        {t(($) => $.detail.external_binding_title)}
      </div>
      {binding === undefined ? (
        <p className="text-micro text-muted-foreground">
          {t(($) => $.detail.external_binding_projection_unavailable)}
        </p>
      ) : binding?.session ? (
        <div className="space-y-1">
          <ExternalSessionLink session={binding.session} detailed />
          {!binding.deliverable && reason && (
            <p className="text-micro text-destructive">{reason}</p>
          )}
          <p className="text-micro text-muted-foreground">
            {t(($) => $.detail.external_binding_future_only)}
          </p>
        </div>
      ) : binding ? (
        <p className="text-micro text-destructive">
          {t(($) => $.detail.external_binding_unavailable)}
        </p>
      ) : (
        <p className="text-micro text-muted-foreground">
          {t(($) => $.detail.external_binding_none)}
        </p>
      )}

      {binding !== undefined && canEdit.allowed && !isLoading && (
        <Dialog
          open={open}
          onOpenChange={(next) => {
            setOpen(next);
          }}
        >
          <DialogTrigger render={<Button variant="outline" size="xs" />}>
            <Link2 className="size-3" />
            {t(($) => $.detail.external_binding_change)}
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t(($) => $.detail.external_binding_choose_title)}</DialogTitle>
              <DialogDescription>
                {t(($) => $.detail.external_binding_choose_description)}
              </DialogDescription>
            </DialogHeader>
            {open && (
              <SessionPicker
                key={binding?.id ?? "unbound"}
                agent={agent}
                issueId={issueId}
                currentSessionId={binding?.session?.id ?? null}
                onSaved={() => setOpen(false)}
              />
            )}
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
