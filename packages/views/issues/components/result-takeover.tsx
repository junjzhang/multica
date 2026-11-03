"use client";

import { useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAgentPermissions } from "@multica/core/permissions";
import { agentListOptions } from "@multica/core/workspace/queries";
import { resultTakeoverOptions, useTakeoverIssueResult } from "@multica/core/issues";
import { createSafeId } from "@multica/core/utils";
import type { AgentTask, ResultNotification, ResultTakeoverChoice, ResultTakeoverRequest } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@multica/ui/components/ui/dialog";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { useT } from "../../i18n";
import { SessionChoice } from "./session-choice";

function choiceKey(choice: ResultTakeoverChoice, currentReturnId: string) {
  const selector = choice.kind === "managed_task" ? choice.source_task_id : choice.external_session_id;
  return JSON.stringify([currentReturnId, choice.kind, selector, choice.provider, choice.session_ref,
    choice.kind === "managed_task" ? choice.selection_version : ""]);
}

type Attempt = { request: ResultTakeoverRequest; label: string; existingReturnIds: string[] };

export function ResultTakeover({ issueId, task, notification, notifications }: {
  issueId: string;
  task: AgentTask;
  notification: ResultNotification;
  notifications: ResultNotification[];
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const agents = useQuery(agentListOptions(wsId));
  const agent = agents.data?.find((candidate) => candidate.id === notification.recipient_agent_id) ?? null;
  const permission = useAgentPermissions(agent, wsId);
  const canManage = permission.canEdit.allowed && !permission.isLoading && !!agent;
  const canChooseNew = canManage && !agent.archived_at;
  const options = useQuery({
    ...resultTakeoverOptions(wsId, issueId, task.id, notification.return_id),
    enabled: canChooseNew,
  });
  const mutation = useTakeoverIssueResult(wsId, issueId, notification.recipient_agent_id);
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState("");
  const [attempt, setAttempt] = useState<Attempt | null>(null);
  const attemptRef = useRef<Attempt | null>(null);
  const [previousUnknown, setPreviousUnknown] = useState(false);
  const data = canChooseNew && !options.isError ? options.data : undefined;
  const target = data?.returns.find((version) => version.return_id === notification.return_id)?.target;
  const selectedChoice = data?.targets.find((choice) => choiceKey(choice, data.current_return_id) === selected && choice.available);
  const conflict = mutation.error instanceof ApiError && mutation.error.status === 409;
  const rejected = mutation.error instanceof ApiError && mutation.error.status >= 400 && mutation.error.status < 500;
  const unknown = mutation.isError && !rejected;
  const unsupported = options.error instanceof ApiError && (options.error.status === 403 || options.error.status === 404);
  const response = mutation.data;
  const existing = response && attempt?.existingReturnIds.includes(response.target_return_id);

  const submit = async () => {
    if (!canManage || mutation.isPending || (!attempt && (!canChooseNew || !selectedChoice || !data))) return;
    let next = attemptRef.current;
    if (!next) {
      if (!selectedChoice || !data) return;
      next = {
        label: selectedChoice.display_name || selectedChoice.session_ref,
        existingReturnIds: notifications.map((note) => note.return_id),
        request: {
          client_request_id: createSafeId(), result_task_id: task.id, from_return_id: notification.return_id,
          expected_current_return_id: data.current_return_id,
          target: selectedChoice.kind === "managed_task"
            ? { kind: "managed_task", source_task_id: selectedChoice.source_task_id, selection_version: selectedChoice.selection_version }
            : { kind: "external_session", external_session_id: selectedChoice.external_session_id },
        },
      };
      attemptRef.current = next;
      setAttempt(next);
    }
    try {
      await mutation.mutateAsync(next.request);
    } catch {
      // Keep the exact request and key for an explicit retry of uncertain input.
    }
  };

  const chooseAgain = () => {
    if (!canChooseNew) return;
    setPreviousUnknown(previousUnknown || unknown);
    attemptRef.current = null;
    setAttempt(null);
    setSelected("");
    mutation.reset();
    void options.refetch();
  };

  return (
    <div className="space-y-1 text-micro text-muted-foreground">
      <p>{notification.return_id === (data?.current_return_id ?? notification.current_return_id)
        ? t(($) => $.result_takeover.current) : t(($) => $.result_takeover.historical)}</p>
      {!target && <p>{t(($) => $.result_takeover.return_reference, { id: notification.return_id.slice(-8) })}</p>}
      <p className="break-words">{t(($) => $.result_takeover.fixed_target)}: {target
        ? `${target.display_name || target.session_ref} · ${target.provider} · ${target.session_ref}`
        : t(($) => $.result_takeover.target_unavailable)}</p>
      {canChooseNew && options.isError && !unsupported && (
        <div>
          <p role="status">{t(($) => $.result_takeover.read_failed)}</p>
          <Button variant="ghost" size="xs" onClick={() => void options.refetch()}>{t(($) => $.result_takeover.reload)}</Button>
        </div>
      )}
      {canManage && (data || attempt) && (!unsupported || attempt) && (
        <Dialog open={open} onOpenChange={(next) => {
          if (mutation.isPending) return;
          if (next && mutation.isSuccess && canChooseNew) { attemptRef.current = null; setAttempt(null); setSelected(""); mutation.reset(); }
          setOpen(next);
        }}>
          <DialogTrigger render={<Button variant="outline" size="xs" />}>
            {canChooseNew ? t(($) => $.result_takeover.action) : t(($) => $.result_takeover.retry)}
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t(($) => $.result_takeover.title)}</DialogTitle>
              <DialogDescription>{t(($) => $.result_takeover.description, { agent: agent?.name ?? "" })}</DialogDescription>
            </DialogHeader>
            <p className="text-caption text-muted-foreground">{t(($) => $.result_takeover.original_notice)}</p>
            {previousUnknown && <p role="status" className="text-caption text-warning">{t(($) => $.result_takeover.previous_unknown)}</p>}
            {attempt ? <p className="break-words text-caption">{t(($) => $.result_takeover.choose)}: {attempt.label}</p> : (
              <div className="max-h-80 space-y-2 overflow-y-auto">
                {options.isFetching && <p role="status" className="text-caption">{t(($) => $.result_takeover.loading)}</p>}
                {options.isError && <p role="alert" className="text-caption">{t(($) => $.result_takeover.read_failed)}</p>}
                {data && selected && !selectedChoice && <p role="status" className="text-caption">{t(($) => $.result_takeover.selection_changed)}</p>}
                {data?.targets.length === 0 && <p className="text-caption">{t(($) => $.result_takeover.empty)}</p>}
                {data && <div role="radiogroup" aria-label={t(($) => $.result_takeover.choose)} className="space-y-1.5">
                  {data.targets.map((choice) => <SessionChoice key={choiceKey(choice, data.current_return_id)} value={choiceKey(choice, data.current_return_id)}
                    name={`result-target-${task.id}-${notification.return_id}`}
                    title={choice.display_name || choice.session_ref} details={`${choice.provider} · ${choice.session_ref}`}
                    available={choice.available && !mutation.isPending && !options.isFetching} selected={selected === choiceKey(choice, data.current_return_id)}
                    onSelect={() => setSelected(choiceKey(choice, data.current_return_id))} />)}
                </div>}
              </div>
            )}
            {mutation.isError && <p role="alert" className="text-caption text-destructive">
              {conflict ? t(($) => $.result_takeover.conflict) : unknown ? t(($) => $.result_takeover.unknown) : t(($) => $.result_takeover.rejected)}
            </p>}
            {response && <div role="status" className="space-y-1 text-caption">
              <p>{existing ? t(($) => $.result_takeover.existing) : t(($) => $.result_takeover.saved)}</p>
              {response.target_return_id !== response.current_return_id && <p>{t(($) => $.result_takeover.route_changed)}</p>}
            </div>}
            <DialogFooter>
              <Button variant="outline" disabled={mutation.isPending} onClick={() => setOpen(false)}>{attempt ? t(($) => $.result_takeover.close) : t(($) => $.result_takeover.cancel)}</Button>
              {canChooseNew && attempt && !mutation.isPending && !response && <Button variant="outline" onClick={chooseAgain}>
                {conflict || rejected ? t(($) => $.result_takeover.reload) : t(($) => $.result_takeover.change_target)}
              </Button>}
              {!response && <Button onClick={() => void submit()} aria-busy={mutation.isPending}
                disabled={!canManage || (!attempt && !canChooseNew) || mutation.isPending || (rejected && !conflict) || (!attempt && (!selectedChoice || options.isFetching || options.isError))}>
                {mutation.isPending && <Spinner aria-hidden className="size-3.5" />}
                {attempt ? t(($) => $.result_takeover.retry) : t(($) => $.result_takeover.submit)}
              </Button>}
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
