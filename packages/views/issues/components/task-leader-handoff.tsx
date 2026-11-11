"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import { agentExecutionKind } from "@multica/core/agents/execution-kind";
import { agentSessionsOptions } from "@multica/core/agents/session-bindings";
import { useWorkspaceId } from "@multica/core/hooks";
import { useHandoffIssueLeadership } from "@multica/core/issues";
import { useCurrentMember } from "@multica/core/permissions";
import type { Agent, Issue } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";
import { agentListOptions, squadMembersOptions } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Label } from "@multica/ui/components/ui/label";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { SessionChoice } from "./session-choice";

/**
 * Human handoff for a task-chain leader. The server requires a workspace
 * owner/admin and CAS on both the leadership and issue revisions; an external
 * target must name one of its registered sessions, a managed target none.
 * Earlier results are not re-routed by the handoff, so the success panel says
 * they need an explicit result takeover.
 */
export function TaskLeaderHandoff({ issue }: { issue: Issue }) {
  const wsId = useWorkspaceId();
  const { t } = useT("issues");
  const { role } = useCurrentMember(wsId);
  const squadId = issue.leadership_squad_id ?? "";
  const { data: members = [] } = useQuery(squadMembersOptions(wsId, squadId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  // CAS and cache invalidation refer to the root even when the card is a subtask.
  const rootIssueId = issue.leadership_root_issue_id ?? issue.id;
  // A subtask's own revision is not the root's; without a projected root
  // revision, send an impossible value so the server rejects the stale request
  // instead of silently comparing against the wrong row.
  const rootRevision =
    issue.leadership_root_revision ?? (rootIssueId === issue.id ? issue.revision ?? 0 : 0);
  const mutation = useHandoffIssueLeadership(wsId, issue.id, rootIssueId);
  const [open, setOpen] = useState(false);
  const [targetId, setTargetId] = useState("");
  const [sessionId, setSessionId] = useState("");
  const [reason, setReason] = useState("");

  const canHandoff = role === "owner" || role === "admin";
  const candidates = useMemo(() => {
    const byId = new Map(agents.map((agent) => [agent.id, agent]));
    const seen = new Set<string>();
    const out: Agent[] = [];
    for (const member of members) {
      if (member.member_type !== "agent" || seen.has(member.member_id)) continue;
      const agent = byId.get(member.member_id);
      if (!agent || agent.archived_at) continue;
      seen.add(member.member_id);
      out.push(agent);
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }, [members, agents]);

  const target = candidates.find((agent) => agent.id === targetId) ?? null;
  const targetExternal = target !== null && agentExecutionKind(target) === "external";
  const sessions = useQuery({
    ...agentSessionsOptions(wsId, targetId),
    enabled: open && !!targetId && targetExternal,
  });
  const selectedSession = sessions.data?.find((session) => session.id === sessionId) ?? null;

  const response = mutation.data;
  const conflict = mutation.error instanceof ApiError && mutation.error.status === 409;
  const forbidden = mutation.error instanceof ApiError && mutation.error.status === 403;
  // The current leader session is not a handoff target; every other
  // registered session is, with its server-computed deliverability.
  const selectableSessions = (sessions.data ?? []).filter(
    (session) => session.id !== issue.leader_external_session_id,
  );
  const missingTarget = !target;
  const missingSession = target !== null && targetExternal && !selectedSession?.deliverable;
  const missingReason = reason.trim().length === 0;
  const submit = () => {
    if (!target || missingSession || missingReason || mutation.isPending || response) return;
    mutation.mutate({
      client_request_id: createSafeId(),
      expected_revision: issue.leadership_revision ?? 0,
      expected_issue_revision: rootRevision,
      previous_agent_id: issue.leader_agent_id ?? "",
      previous_session_id: issue.leader_external_session_id || undefined,
      agent_id: target.id,
      session_id: targetExternal ? selectedSession?.id : undefined,
      reason: reason.trim(),
    });
  };

  if (!issue.leadership_root_issue_id || !issue.leader_agent_id || !squadId || !canHandoff) {
    return null;
  }

  return (
    <div className="space-y-1">
      <Button
        type="button"
        variant="outline"
        size="xs"
        onClick={() => {
          setOpen(true);
          setTargetId("");
          setSessionId("");
          setReason("");
          mutation.reset();
        }}
      >
        {t(($) => $.detail.task_leader_handoff)}
      </Button>
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (mutation.isPending) return;
          setOpen(next);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t(($) => $.detail.task_leader_handoff_title)}</DialogTitle>
            <DialogDescription>{t(($) => $.detail.task_leader_handoff_description)}</DialogDescription>
          </DialogHeader>

          {response ? (
            <div role="status" className="space-y-1 text-caption">
              <p className="font-medium">{t(($) => $.detail.task_leader_handoff_success)}</p>
              {response.stale_contract_ids.length > 0 && (
                <p>{t(($) => $.detail.task_leader_handoff_stale_contracts, { count: response.stale_contract_ids.length })}</p>
              )}
              {response.advanced_returns.length > 0 && (
                <p>{t(($) => $.detail.task_leader_handoff_advanced_returns, { count: response.advanced_returns.length })}</p>
              )}
              <p className="text-muted-foreground">{t(($) => $.detail.task_leader_handoff_historical_results)}</p>
            </div>
          ) : (
            <div className="space-y-3">
              <p className="text-caption text-muted-foreground">
                {t(($) => $.detail.task_leader_handoff_revisions, {
                  leadership: issue.leadership_revision ?? 0,
                  issue: rootRevision,
                })}
              </p>
              <div className="space-y-1.5">
                <Label>{t(($) => $.detail.task_leader_handoff_target)}</Label>
                {candidates.length === 0 ? (
                  <p className="text-caption text-muted-foreground">{t(($) => $.detail.task_leader_handoff_no_targets)}</p>
                ) : (
                  <div role="radiogroup" aria-label={t(($) => $.detail.task_leader_handoff_target)} className="max-h-56 space-y-1 overflow-y-auto">
                    {candidates.map((agent) => {
                      const external = agentExecutionKind(agent) === "external";
                      const selected = agent.id === targetId;
                      return (
                        <label
                          key={agent.id}
                          className={cn(
                            "flex w-full min-w-0 cursor-pointer flex-col gap-0.5 rounded-md border p-2 text-left text-caption transition-colors focus-within:ring-2 focus-within:ring-ring/50",
                            selected ? "border-primary bg-primary/5" : "border-border/70 hover:bg-muted/50",
                          )}
                        >
                          <input
                            type="radio"
                            name={`handoff-target-${issue.id}`}
                            value={agent.id}
                            checked={selected}
                            onChange={() => {
                              setTargetId(agent.id);
                              setSessionId("");
                            }}
                            className="sr-only"
                          />
                          <span className="min-w-0 truncate font-medium">{agent.name}</span>
                          <span className="text-micro text-muted-foreground">
                            {external
                              ? t(($) => $.detail.task_leader_handoff_external)
                              : t(($) => $.detail.task_leader_handoff_managed)}
                          </span>
                        </label>
                      );
                    })}
                  </div>
                )}
              </div>

              {targetExternal && (
                <div className="space-y-1.5">
                  <Label>{t(($) => $.detail.task_leader_handoff_session)}</Label>
                  {sessions.isFetching && <p role="status" className="text-caption">{t(($) => $.detail.task_leader_handoff_session_placeholder)}</p>}
                  {!sessions.isFetching && selectableSessions.length === 0 && (
                    <p className="text-caption text-muted-foreground">{t(($) => $.detail.task_leader_handoff_no_sessions)}</p>
                  )}
                  <div role="radiogroup" aria-label={t(($) => $.detail.task_leader_handoff_session)} className="max-h-56 space-y-1 overflow-y-auto">
                    {selectableSessions.map((session) => (
                      <SessionChoice
                        key={session.id}
                        value={session.id}
                        name={`handoff-session-${issue.id}`}
                        title={session.display_name || session.session_id}
                        details={`${session.provider} · ${session.session_id}`}
                        available={session.deliverable}
                        reason={session.deliverable ? undefined : t(($) => $.detail.task_leader_handoff_session_unavailable)}
                        selected={session.id === sessionId}
                        onSelect={() => setSessionId(session.id)}
                      />
                    ))}
                  </div>
                </div>
              )}

              <div className="space-y-1.5">
                <Label htmlFor={`handoff-reason-${issue.id}`}>{t(($) => $.detail.task_leader_handoff_reason)}</Label>
                <Textarea
                  id={`handoff-reason-${issue.id}`}
                  value={reason}
                  placeholder={t(($) => $.detail.task_leader_handoff_reason_placeholder)}
                  onChange={(event) => setReason(event.target.value)}
                />
              </div>

              {(missingTarget || missingSession || missingReason) && (targetId || sessionId || reason) && (
                <p role="status" className="text-caption text-muted-foreground">
                  {missingTarget
                    ? t(($) => $.detail.task_leader_handoff_missing_target)
                    : missingSession
                      ? t(($) => $.detail.task_leader_handoff_missing_session)
                      : t(($) => $.detail.task_leader_handoff_missing_reason)}
                </p>
              )}
              {mutation.isError && (
                <p role="alert" className="text-caption text-destructive">
                  {forbidden
                    ? t(($) => $.detail.task_leader_handoff_forbidden)
                    : conflict
                      ? t(($) => $.detail.task_leader_handoff_conflict)
                      : t(($) => $.detail.task_leader_handoff_error)}
                </p>
              )}
            </div>
          )}

          <DialogFooter>
            <Button variant="outline" disabled={mutation.isPending} onClick={() => setOpen(false)}>
              {response ? t(($) => $.detail.task_leader_handoff_close) : t(($) => $.detail.task_leader_handoff_cancel)}
            </Button>
            {!response && (
              <Button
                onClick={submit}
                aria-busy={mutation.isPending}
                disabled={mutation.isPending || !target || missingSession || missingReason}
              >
                {mutation.isPending && <Spinner aria-hidden className="size-3.5" />}
                {t(($) => $.detail.task_leader_handoff_submit)}
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
