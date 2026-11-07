"use client";

import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, FileCheck2 } from "lucide-react";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  issueContractOptions,
  useUpdateIssueContract,
} from "@multica/core/issues/contract";
import { childIssuesOptions } from "@multica/core/issues/queries";
import type {
  ContractRequest,
  ContractView,
} from "@multica/core/api/issue-contract-schema";
import { useActorName } from "@multica/core/workspace/hooks";
import { Button } from "@multica/ui/components/ui/button";
import { Textarea } from "@multica/ui/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@multica/ui/components/ui/dialog";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT } from "../../i18n";

type Actor = NonNullable<ContractView["pending_actor"]>;

export function IssueContract({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const { t } = useT("issues");
  const query = useQuery(issueContractOptions(wsId, issueId));
  if (query.isPending) return null;
  if (query.isError && !query.data)
    return (
      <div className="space-y-2 px-2 text-caption">
        <p>{t(($) => $.contract.load_error)}</p>
        <Button size="xs" variant="outline" onClick={() => query.refetch()}>
          {t(($) => $.contract.retry)}
        </Button>
      </div>
    );
  if (!query.data) return null;
  return (
    <ContractSection
      key={issueId}
      issueId={issueId}
      view={query.data}
      refreshing={query.isFetching}
      refresh={() => query.refetch({ throwOnError: true })}
    />
  );
}

function ContractSection({
  issueId,
  view,
  refresh,
  refreshing,
}: {
  issueId: string;
  view: ContractView;
  refresh: () => Promise<unknown>;
  refreshing: boolean;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const mutation = useUpdateIssueContract(wsId, issueId);
  const fieldId = useId();
  const { getActorName } = useActorName();
  const [open, setOpen] = useState(true);
  const [editOpen, setEditOpen] = useState(false);
  const [selected, setSelected] = useState<number[]>([]);
  const [evidence, setEvidence] = useState("");
  const [reason, setReason] = useState("");
  const [children, setChildren] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [conflict, setConflict] = useState(false);
  const [retry, setRetry] = useState<ContractRequest | null>(null);
  const [draftBasis, setDraftBasis] = useState<{
    sequence: number;
    version: number;
  } | null>(null);
  const state = view.contract;
  const childQuery = useQuery({
    ...childIssuesOptions(wsId, issueId),
    enabled: view.can_submit,
  });
  const version = state?.versions.at(-1);
  const pending = state?.deliveries.findLast(
    (delivery) =>
      delivery.version === version?.number &&
      !state.decisions.some((decision) => decision.delivery_id === delivery.id),
  );
  const allowed = view.can_review
    ? (pending?.criteria ?? [])
    : (version?.criteria.flatMap((_, i) =>
        view.accepted.includes(i) ? [] : [i],
      ) ?? []);

  const draftCurrent =
    !draftBasis ||
    (draftBasis.sequence === (state?.sequence ?? 0) &&
      draftBasis.version === (version?.number ?? 0));
  const needsRestart = conflict || (!retry && !draftCurrent);
  const locked = mutation.isPending || needsRestart || !!retry;
  async function discardDraft() {
    try {
      await refresh();
      setRetry(null);
      setConflict(false);
      setDraftBasis(null);
      setSelected([]);
      setChildren([]);
      setEvidence("");
      setReason("");
      setError("");
      setEditOpen(false);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : t(($) => $.contract.load_error),
      );
    }
  }
  const beginDraft = () =>
    setDraftBasis(
      (basis) =>
        basis ?? {
          sequence: state?.sequence ?? 0,
          version: version?.number ?? 0,
        },
    );
  const toggleCriterion = (index: number, checked: boolean) => {
    beginDraft();
    setSelected((items) =>
      checked ? [...items, index] : items.filter((item) => item !== index),
    );
  };
  const toggleChild = (id: string, checked: boolean) => {
    beginDraft();
    setChildren((items) =>
      checked ? [...items, id] : items.filter((item) => item !== id),
    );
  };
  const identity = (actor: Actor) => (
    <span className="flex min-w-0 items-center gap-1.5">
      <ActorAvatar actorType={actor.type} actorId={actor.id} size="sm" />
      <span className="break-words">{getActorName(actor.type, actor.id)}</span>
    </span>
  );
  async function apply(
    input: Omit<
      ContractRequest,
      "client_request_id" | "expected_sequence" | "version"
    >,
  ) {
    const request: ContractRequest = {
      ...input,
      client_request_id: crypto.randomUUID(),
      expected_sequence: draftBasis?.sequence ?? state?.sequence ?? 0,
      version: draftBasis?.version ?? version?.number ?? 0,
    };
    await send(request);
  }
  async function send(request: ContractRequest) {
    setError("");
    setRetry(request);
    try {
      await mutation.mutateAsync(request);
      setRetry(null);
      setConflict(false);
      setDraftBasis(null);
      setEditOpen(false);
      setSelected([]);
      setChildren([]);
      setEvidence("");
      setReason("");
    } catch (err) {
      const rejected =
        err instanceof ApiError && err.status >= 400 && err.status < 500;
      setConflict(rejected && err.status === 409);
      if (rejected) setRetry(null);
      setError(
        err instanceof Error ? err.message : t(($) => $.contract.save_error),
      );
    }
  }

  const recovery = (error || needsRestart) && (
    <div role="alert" className="space-y-2 text-caption text-destructive">
      <p className="break-words">
        {error || t(($) => $.contract.draft_changed)}
      </p>
      {needsRestart ? (
        <Button
          type="button"
          size="xs"
          variant="outline"
          disabled={refreshing || mutation.isPending}
          aria-busy={refreshing}
          onClick={discardDraft}
        >
          {t(($) => $.contract.restart_draft)}
        </Button>
      ) : (
        retry && (
          <Button
            type="button"
            size="xs"
            variant="outline"
            disabled={mutation.isPending}
            onClick={() => send(retry)}
          >
            {t(($) => $.contract.retry)}
          </Button>
        )
      )}
    </div>
  );

  return (
    <section
      aria-label={t(($) => $.contract.title)}
      className="min-w-0 space-y-3"
      data-issue-contract
    >
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className="flex w-full items-center gap-1 px-2 text-caption font-medium text-muted-foreground"
      >
        <FileCheck2 className="size-3" />
        {t(($) => $.contract.title)}
        <ChevronRight
          className={`ml-auto size-3 transition-transform ${open ? "rotate-90" : ""}`}
        />
      </button>
      {open && (
        <div className="min-w-0 space-y-3 px-2 text-caption">
          {version ? (
            <>
              <div className="flex items-center justify-between gap-2">
                <span>
                  {t(($) => $.contract.version, { version: version.number })}
                </span>
                <strong>
                  {t(($) => $.contract.progress, {
                    accepted: view.accepted.length,
                    total: version.criteria.length,
                  })}
                </strong>
              </div>
              <div className="space-y-2">
                <div className="text-muted-foreground">
                  {t(($) => $.contract.executor)}
                </div>
                {view.executor
                  ? identity(view.executor)
                  : t(($) => $.contract.unassigned)}
                <div className="text-muted-foreground">
                  {t(($) => $.contract.acceptor)}
                </div>
                {view.acceptor
                  ? identity(view.acceptor)
                  : t(($) => $.contract.unassigned)}
              </div>
              <div
                className="rounded-md bg-accent/50 p-2 space-y-1"
                role="status"
              >
                <div className="font-medium">
                  {view.stale
                    ? t(($) => $.contract.needs_revision)
                    : view.pending_actor
                      ? pending
                        ? t(($) => $.contract.waiting_review)
                        : t(($) => $.contract.waiting_delivery)
                      : view.accepted.length === version.criteria.length
                        ? t(($) => $.contract.accepted)
                        : t(($) => $.contract.paused)}
                </div>
                {view.pending_actor && identity(view.pending_actor)}
              </div>
              <details>
                <summary className="cursor-pointer font-medium">
                  {t(($) => $.contract.goal_scope)}
                </summary>
                <p className="whitespace-pre-wrap break-words pt-2">
                  {version.goal}
                </p>
                {version.scope && (
                  <p className="whitespace-pre-wrap break-words pt-2">
                    {version.scope}
                  </p>
                )}
              </details>
              <div className="space-y-2">
                {version.criteria.map((criterion, i) => (
                  <label key={i} className="flex items-start gap-2 break-words">
                    <input
                      type="checkbox"
                      className="mt-0.5 shrink-0"
                      checked={
                        view.accepted.includes(i) ||
                        (draftCurrent && selected.includes(i))
                      }
                      disabled={
                        locked ||
                        !allowed.includes(i) ||
                        (!view.can_submit && !view.can_review)
                      }
                      onChange={(event) =>
                        toggleCriterion(i, event.target.checked)
                      }
                    />
                    <span
                      className={`min-w-0 whitespace-pre-wrap ${view.accepted.includes(i) ? "text-muted-foreground" : ""}`}
                    >
                      {criterion}
                    </span>
                  </label>
                ))}
              </div>
              <details open>
                <summary className="cursor-pointer font-medium">
                  {t(($) => $.contract.authorization)}
                </summary>
                <p className="whitespace-pre-wrap break-words pt-2">
                  {version.authorization_note}
                </p>
                <p className="pt-2 text-muted-foreground">
                  {t(($) => $.contract.authorization_note_help)}
                </p>
                <p className="pt-2 [overflow-wrap:anywhere]">
                  {view.merge_authorization.allowed
                    ? t(($) => $.contract.merge_allowed, {
                        repository:
                          view.merge_authorization.repository_url ?? "",
                        branch: view.merge_authorization.target_branch ?? "",
                      })
                    : t(($) => $.contract.merge_not_allowed)}
                </p>
              </details>
              {pending && (
                <div className="space-y-1">
                  <div className="font-medium">
                    {t(($) => $.contract.evidence)}
                  </div>
                  <p className="whitespace-pre-wrap break-words">
                    {pending.evidence}
                  </p>
                </div>
              )}
              {view.can_submit && (
                <div className="space-y-2">
                  <div className="space-y-1">
                    <label htmlFor={`${fieldId}-evidence`}>
                      {t(($) => $.contract.evidence)}
                    </label>
                    <Textarea
                      disabled={locked}
                      id={`${fieldId}-evidence`}
                      value={evidence}
                      onChange={(event) => {
                        const value = event.target.value;
                        beginDraft();
                        setEvidence(value);
                      }}
                    />
                  </div>
                  {childQuery.data
                    ?.filter(
                      (child) =>
                        child.status === "in_review" &&
                        child.coordinator?.type === version.acceptor.type &&
                        child.coordinator.id === version.acceptor.id,
                    )
                    .map((child) => (
                      <label key={child.id} className="flex items-start gap-2">
                        <input
                          type="checkbox"
                          disabled={locked}
                          checked={children.includes(child.id)}
                          onChange={(event) =>
                            toggleChild(child.id, event.target.checked)
                          }
                        />
                        <span className="break-words">
                          {t(($) => $.contract.cover_child, {
                            title: `${child.identifier} ${child.title}`,
                          })}
                        </span>
                      </label>
                    ))}
                  <Button
                    size="sm"
                    disabled={locked || !selected.length || !evidence.trim()}
                    aria-busy={mutation.isPending}
                    onClick={() =>
                      apply({
                        action: "submit",
                        criteria: selected,
                        evidence,
                        child_ids: children,
                      })
                    }
                  >
                    {t(($) => $.contract.submit)}
                  </Button>
                </div>
              )}
              {view.can_review && pending && (
                <div className="space-y-2">
                  {pending.child_ids.map((id) => (
                    <label className="flex items-start gap-2" key={id}>
                      <input
                        type="checkbox"
                        disabled={locked}
                        checked={children.includes(id)}
                        onChange={(event) =>
                          toggleChild(id, event.target.checked)
                        }
                      />
                      <span className="break-all">
                        {t(($) => $.contract.close_child, { id })}
                      </span>
                    </label>
                  ))}
                  <div className="space-y-1">
                    <label htmlFor={`${fieldId}-review`}>
                      {t(($) => $.contract.review_note)}
                    </label>
                    <Textarea
                      disabled={locked}
                      id={`${fieldId}-review`}
                      value={reason}
                      onChange={(event) => {
                        const value = event.target.value;
                        beginDraft();
                        setReason(value);
                      }}
                    />
                  </div>
                  <div className="flex flex-wrap gap-2">
                    <Button
                      size="sm"
                      disabled={locked || !selected.length}
                      aria-busy={mutation.isPending}
                      onClick={() =>
                        apply({
                          action: "accept",
                          delivery_id: pending.id,
                          criteria: selected,
                          reason,
                          child_ids: children,
                        })
                      }
                    >
                      {t(($) => $.contract.accept_selected)}
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={locked || !reason.trim()}
                      onClick={() =>
                        apply({
                          action: "return",
                          delivery_id: pending.id,
                          reason,
                        })
                      }
                    >
                      {t(($) => $.contract.return_delivery)}
                    </Button>
                  </div>
                </div>
              )}
              <details>
                <summary className="cursor-pointer font-medium">
                  {t(($) => $.contract.history)}
                </summary>
                <div className="space-y-3 pt-2">
                  {state?.versions.map((v) => (
                    <details key={v.number}>
                      <summary className="cursor-pointer">
                        {t(($) => $.contract.version, { version: v.number })} ·{" "}
                        {v.created_at.slice(0, 10)}
                      </summary>
                      <p className="whitespace-pre-wrap break-words">
                        {v.goal}
                      </p>
                      <p className="whitespace-pre-wrap break-words">
                        {v.scope}
                      </p>
                      <p className="whitespace-pre-wrap break-words">
                        {v.authorization_note}
                      </p>
                      <ol className="list-decimal pl-4">
                        {v.criteria.map((c, i) => (
                          <li key={i} className="break-words">
                            {c}
                          </li>
                        ))}
                      </ol>
                    </details>
                  ))}
                  {state?.deliveries.map((d) => (
                    <div className="space-y-1" key={d.id}>
                      <strong>
                        {t(($) => $.contract.version, { version: d.version })} ·{" "}
                        {t(($) => $.contract.evidence)}
                      </strong>
                      {identity(d.author)}
                      <p className="whitespace-pre-wrap break-words">
                        {d.evidence}
                      </p>
                      {state.decisions
                        .filter((decision) => decision.delivery_id === d.id)
                        .map((decision) => (
                          <div key={decision.delivery_id}>
                            {identity(decision.author)}
                            <p>
                              {decision.outcome === "accept"
                                ? t(($) => $.contract.accepted_items, {
                                    accepted: decision.criteria.length,
                                  })
                                : decision.outcome === "return"
                                  ? t(($) => $.contract.returned)
                                  : t(($) => $.contract.unknown_decision)}
                            </p>
                            <p className="whitespace-pre-wrap break-words">
                              {decision.reason}
                            </p>
                          </div>
                        ))}
                    </div>
                  ))}
                </div>
              </details>
            </>
          ) : (
            <p className="text-muted-foreground">
              {t(($) => $.contract.no_contract)}
            </p>
          )}
          {view.can_edit && (
            <Dialog
              open={editOpen}
              onOpenChange={(next) => {
                if (next && locked) return;
                setEditOpen(next);
                if (next)
                  setDraftBasis({
                    sequence: state?.sequence ?? 0,
                    version: version?.number ?? 0,
                  });
              }}
            >
              <DialogTrigger
                disabled={locked}
                render={<Button variant="outline" size="xs" />}
              >
                {version
                  ? t(($) => $.contract.revise)
                  : t(($) => $.contract.create)}
              </DialogTrigger>
              <DialogContent className="max-h-[90dvh] overflow-y-auto">
                <DialogHeader>
                  <DialogTitle>
                    {version
                      ? t(($) => $.contract.revise)
                      : t(($) => $.contract.create)}
                  </DialogTitle>
                </DialogHeader>
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.contract.revision_consequence)}
                </p>
                <form
                  onSubmit={(event) => {
                    event.preventDefault();
                    const data = new FormData(event.currentTarget);
                    void apply({
                      action: "revise",
                      goal: String(data.get("goal")),
                      scope: String(data.get("scope")),
                      authorization_note: String(data.get("authorization")),
                      requirements: String(data.get("requirements"))
                        .split("\n")
                        .map((line) => line.trim())
                        .filter(Boolean),
                    });
                  }}
                  className="space-y-3"
                >
                  <div className="space-y-1 text-caption">
                    <label htmlFor={`${fieldId}-goal`}>
                      {t(($) => $.contract.goal)}
                    </label>
                    <Textarea
                      disabled={locked}
                      id={`${fieldId}-goal`}
                      name="goal"
                      required
                      defaultValue={version?.goal}
                    />
                  </div>
                  <div className="space-y-1 text-caption">
                    <label htmlFor={`${fieldId}-scope`}>
                      {t(($) => $.contract.scope)}
                    </label>
                    <Textarea
                      disabled={locked}
                      id={`${fieldId}-scope`}
                      name="scope"
                      defaultValue={version?.scope}
                    />
                  </div>
                  <div className="space-y-1 text-caption">
                    <label htmlFor={`${fieldId}-requirements`}>
                      {t(($) => $.contract.criteria_lines)}
                    </label>
                    <Textarea
                      disabled={locked}
                      id={`${fieldId}-requirements`}
                      name="requirements"
                      required
                      defaultValue={version?.criteria.join("\n")}
                    />
                  </div>
                  <div className="space-y-1 text-caption">
                    <label htmlFor={`${fieldId}-authorization`}>
                      {t(($) => $.contract.authorization)}
                    </label>
                    <Textarea
                      disabled={locked}
                      id={`${fieldId}-authorization`}
                      name="authorization"
                      required
                      defaultValue={version?.authorization_note}
                    />
                  </div>
                  <p className="text-caption text-muted-foreground">
                    {t(($) => $.contract.identities_help)}
                  </p>
                  {recovery}
                  <Button
                    type="submit"
                    disabled={locked}
                    aria-busy={mutation.isPending}
                  >
                    {t(($) => $.contract.save_version)}
                  </Button>
                </form>
              </DialogContent>
            </Dialog>
          )}
          {!editOpen && recovery}
        </div>
      )}
    </section>
  );
}
