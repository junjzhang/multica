"use client";

import { useMemo, useState } from "react";
import { Lock, UserMinus } from "@multica/ui/icons";
import type { Agent, IssueAssigneeType, UpdateIssueRequest } from "@multica/core/types";
import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "@multica/core/auth";
import {
  agentExecutionKind,
  agentNeedsRuntime,
} from "@multica/core/agents/execution-kind";
import { canAssignAgentToIssue } from "@multica/core/permissions";
import { useActorName } from "@multica/core/workspace/hooks";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions, agentListOptions, squadListOptions, assigneeFrequencyOptions } from "@multica/core/workspace/queries";
import { ActorAvatar } from "../../../common/actor-avatar";
import { DeferredPopup } from "../../../common/deferred-popup";
import {
  PropertyPicker,
  PickerPanel,
  PickerItem,
  PickerSection,
  PickerEmpty,
  PICKER_TRIGGER_CLASS,
} from "./property-picker";
import { useT } from "../../../i18n";
import { AgentSessionChoices } from "../agent-session-choices";
import { Button } from "@multica/ui/components/ui/button";
import { matchesPinyin } from "../../../editor/extensions/pinyin-match";

/**
 * Legacy boolean shape kept around for callers (e.g. `use-issue-actions.ts`)
 * that haven't migrated to the new `canAssignAgentToIssue` Decision API yet.
 * Internally redirects to the canonical rule so behaviour stays in sync.
 */
export function canAssignAgent(
  agent: Agent,
  userId: string | undefined,
  memberRole: string | undefined,
): boolean {
  return canAssignAgentToIssue(agent, {
    userId: userId ?? null,
    role: memberRole === "owner" || memberRole === "admin" || memberRole === "member"
      ? memberRole
      : null,
  }).allowed;
}

interface AssigneePickerProps {
  assigneeType: IssueAssigneeType | null;
  assigneeId: string | null;
  /**
   * `true` when a batch selection spans different assignees ("mixed"): no row
   * is checked, including the unassigned row. Distinct from `assigneeType` /
   * `assigneeId` both being `null`, which means every selected issue is
   * genuinely unassigned and the unassigned row should be checked.
   */
  mixed?: boolean;
  onUpdate: (updates: Partial<UpdateIssueRequest>) => void;
  trigger?: React.ReactNode;
  triggerRender?: React.ReactElement<Record<string, unknown>>;
  /** Hover tooltip naming the property; hidden while the picker is open. */
  tooltip?: React.ReactNode;
  open?: boolean;
  onOpenChange?: (v: boolean) => void;
  align?: "start" | "center" | "end";
}

/**
 * Mounting the real picker subscribes to members/agents/squads/frequency
 * queries — multiplied per board card / list row that cost froze tab
 * switches. Uncontrolled callers that bring their own trigger content get a
 * deferred lookalike trigger instead; the picker mounts on first interaction.
 * The default trigger needs `getActorName` (a members/agents subscription
 * itself), so trigger-less callers stay eager.
 */
export function AssigneePicker(props: AssigneePickerProps) {
  const hasDeferredTriggerContent =
    props.trigger !== undefined || props.triggerRender?.props.children != null;
  const canDefer =
    props.open === undefined &&
    props.onOpenChange === undefined &&
    hasDeferredTriggerContent;
  if (!canDefer) {
    return <AssigneePickerImpl {...props} />;
  }
  return (
    <DeferredPopup
      trigger={props.trigger}
      triggerRender={props.triggerRender}
      triggerClassName={PICKER_TRIGGER_CLASS}
      tooltip={props.tooltip}
    >
      {(open, onOpenChange) => (
        <AssigneePickerImpl {...props} open={open} onOpenChange={onOpenChange} />
      )}
    </DeferredPopup>
  );
}

function AssigneePickerImpl({
  assigneeType,
  assigneeId,
  mixed = false,
  onUpdate,
  trigger: customTrigger,
  triggerRender,
  tooltip,
  open: controlledOpen,
  onOpenChange: controlledOnOpenChange,
  align,
}: AssigneePickerProps) {
  const { t } = useT("issues");
  const [internalOpen, setInternalOpen] = useState(false);
  const open = controlledOpen ?? internalOpen;
  const setOpen = controlledOnOpenChange ?? setInternalOpen;
  const [filter, setFilter] = useState("");
  const [recipient, setRecipient] = useState<ExternalRecipient | null>(null);
  const choices = useAssigneeChoices(filter);
  const { getActorName } = useActorName();
  const pick = (updates: Partial<UpdateIssueRequest>) => {
    onUpdate(updates);
    setRecipient(null);
    setOpen(false);
  };

  const triggerLabel =
    assigneeType && assigneeId
      ? getActorName(assigneeType, assigneeId)
      : t(($) => $.pickers.assignee.trigger_unassigned);

  return (
    <PropertyPicker
      open={open}
      onOpenChange={(v: boolean) => {
        setOpen(v);
        if (!v) { setFilter(""); setRecipient(null); }
      }}
      width="w-64"
      align={align}
      searchable={!recipient}
      searchPlaceholder={t(($) => $.pickers.assignee.search_placeholder)}
      onSearchChange={setFilter}
      triggerRender={triggerRender}
      tooltip={tooltip}
      trigger={
        customTrigger ? customTrigger : assigneeType && assigneeId ? (
          <>
            <ActorAvatar actorType={assigneeType} actorId={assigneeId} size="sm" enableHoverCard showStatusDot />
            <span className="truncate">{triggerLabel}</span>
          </>
        ) : (
          <span className="text-muted-foreground">{t(($) => $.pickers.assignee.trigger_unassigned)}</span>
        )
      }
    >
      {recipient ? (
        <ExternalRecipientStep recipient={recipient} onPick={pick} onCancel={() => setRecipient(null)} />
      ) : (
        <AssigneeOptions
          choices={choices}
          filter={filter}
          assigneeType={assigneeType}
          assigneeId={assigneeId}
          mixed={mixed}
          onPick={pick}
          onChooseExternal={setRecipient}
        />
      )}
    </PropertyPicker>
  );
}

/**
 * The assignee picker's search box and list without the popover, for hosting
 * inside a menu. `onPicked` runs after the choice is applied so the host can
 * close itself.
 */
export function AssigneePickerPanel({
  assigneeType,
  assigneeId,
  onUpdate,
  onPicked,
}: {
  assigneeType: IssueAssigneeType | null;
  assigneeId: string | null;
  onUpdate: (updates: Partial<UpdateIssueRequest>) => void;
  onPicked: () => void;
}) {
  const { t } = useT("issues");
  const [filter, setFilter] = useState("");
  const [recipient, setRecipient] = useState<ExternalRecipient | null>(null);
  const choices = useAssigneeChoices(filter);
  const pick = (updates: Partial<UpdateIssueRequest>) => {
    onUpdate(updates);
    setRecipient(null);
    onPicked();
  };

  return (
    <PickerPanel
      searchable={!recipient}
      autoFocusSearch
      searchPlaceholder={t(($) => $.pickers.assignee.search_placeholder)}
      onSearchChange={setFilter}
    >
      {recipient ? (
        <ExternalRecipientStep recipient={recipient} onPick={pick} onCancel={() => setRecipient(null)} />
      ) : (
        <AssigneeOptions
          choices={choices}
          filter={filter}
          assigneeType={assigneeType}
          assigneeId={assigneeId}
          onPick={pick}
          onChooseExternal={setRecipient}
        />
      )}
    </PickerPanel>
  );
}

function useAssigneeChoices(filter: string) {
  const user = useAuthStore((s) => s.user);
  const wsId = useWorkspaceId();
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: squads = [] } = useQuery(squadListOptions(wsId));
  const { data: frequency = [] } = useQuery(assigneeFrequencyOptions(wsId));

  const currentMember = members.find((m) => m.user_id === user?.id);
  const memberRole = currentMember?.role;

  // Build a lookup map from frequency data for sorting.
  const freqMap = useMemo(() => {
    const map = new Map<string, number>();
    for (const entry of frequency) {
      map.set(`${entry.assignee_type}:${entry.assignee_id}`, entry.frequency);
    }
    return map;
  }, [frequency]);

  const getFreq = (type: string, id: string) => freqMap.get(`${type}:${id}`) ?? 0;

  const query = filter.trim().toLowerCase();
  const filteredMembers = members
    .filter((m) => m.name.toLowerCase().includes(query) || matchesPinyin(m.name, query))
    .sort((a, b) => getFreq("member", b.user_id) - getFreq("member", a.user_id));
  const filteredAgents = agents
    .filter((a) => !a.archived_at && (a.name.toLowerCase().includes(query) || matchesPinyin(a.name, query)))
    .sort((a, b) => getFreq("agent", b.id) - getFreq("agent", a.id));
  const filteredSquads = squads
    .filter((s) => !s.archived_at && (s.name.toLowerCase().includes(query) || matchesPinyin(s.name, query)))
    .sort((a, b) => getFreq("squad", b.id) - getFreq("squad", a.id));
  // Execution boundary per agent, not mere runtime presence: an external
  // identity needs no runtime and keeps its session presentation, an unknown
  // kind exposes no executable action, and a managed agent without a runtime
  // keeps its real repair signal.
  const liveAgentsById = new Map(
    agents.filter((agent) => !agent.archived_at).map((agent) => [agent.id, agent]),
  );

  return {
    userId: user?.id ?? null,
    memberRole,
    filteredMembers,
    filteredAgents,
    filteredSquads,
    liveAgentsById,
  };
}

/** An agent or squad whose executor is external: assigning it picks a session first. */
interface ExternalRecipient {
  type: "agent" | "squad";
  id: string;
  agentId: string;
}

function ExternalRecipientStep({
  recipient,
  onPick,
  onCancel,
}: {
  recipient: ExternalRecipient;
  onPick: (updates: Partial<UpdateIssueRequest>) => void;
  onCancel: () => void;
}) {
  const { t } = useT("issues");
  const [sessionId, setSessionId] = useState("");
  const assignee = { assignee_type: recipient.type, assignee_id: recipient.id };
  return (
    <div className="space-y-2 p-2">
      <p className="text-caption font-medium">{t(($) => $.detail.external_binding_choose_title)}</p>
      <AgentSessionChoices agentId={recipient.agentId} selected={sessionId} onSelect={setSessionId} />
      <div className="flex flex-wrap justify-end gap-2">
        <Button type="button" size="sm" variant="outline" onClick={onCancel}>{t(($) => $.detail.external_binding_cancel)}</Button>
        <Button type="button" size="sm" disabled={!sessionId} onClick={() => onPick({ ...assignee, recipient_session_id: sessionId })}>
          {t(($) => $.detail.external_binding_save)}
        </Button>
        <Button type="button" size="sm" variant="ghost" className="w-full" onClick={() => onPick({ ...assignee, suppress_run: true })}>
          {t(($) => $.pickers.assignee.assign_without_starting)}
        </Button>
      </div>
    </div>
  );
}

function agentActionState(agent: Agent) {
  const kind = agentExecutionKind(agent);
  if (kind === "unknown") return "unsupported" as const;
  if (kind === "external") return "ready" as const;
  return agentNeedsRuntime(agent) ? ("needs-runtime" as const) : ("ready" as const);
}

function AssigneeOptions({
  choices,
  filter,
  assigneeType,
  assigneeId,
  mixed = false,
  onPick,
  onChooseExternal,
}: {
  choices: ReturnType<typeof useAssigneeChoices>;
  filter: string;
  assigneeType: IssueAssigneeType | null;
  assigneeId: string | null;
  mixed?: boolean;
  onPick: (updates: Partial<UpdateIssueRequest>) => void;
  onChooseExternal: (recipient: ExternalRecipient) => void;
}) {
  const { t } = useT("issues");
  const { userId, memberRole, filteredMembers, filteredAgents, filteredSquads, liveAgentsById } =
    choices;

  const isSelected = (type: string, id: string) =>
    assigneeType === type && assigneeId === id;

  const chooseAssignee = (type: "agent" | "squad", id: string, agent: Agent) => {
    if (agentExecutionKind(agent) === "external") {
      onChooseExternal({ type, id, agentId: agent.id });
      return;
    }
    onPick({ assignee_type: type, assignee_id: id });
  };

  return (
    <>
      {/* Unassigned — always the first row, search active or not. Every
          picker in the app puts the empty value there, so "clear this field"
          never moves. */}
      <PickerItem
        emptyValue
        selected={!mixed && !assigneeType && !assigneeId}
        onClick={() => onPick({ assignee_type: null, assignee_id: null })}
      >
        <UserMinus className="h-3.5 w-3.5 text-muted-foreground" />
        <span className="text-muted-foreground">{t(($) => $.pickers.assignee.trigger_unassigned)}</span>
      </PickerItem>

      {/* Members */}
      {filteredMembers.length > 0 && (
        <PickerSection label={t(($) => $.pickers.assignee.members_group)}>
          {filteredMembers.map((m) => (
            <PickerItem
              key={m.user_id}
              selected={isSelected("member", m.user_id)}
              onClick={() =>
                onPick({
                  assignee_type: "member",
                  assignee_id: m.user_id,
                })
              }
            >
              <ActorAvatar actorType="member" actorId={m.user_id} size="sm" />
              <span className="truncate">{m.name}</span>
            </PickerItem>
          ))}
        </PickerSection>
      )}

      {/* Agents */}
      {filteredAgents.length > 0 && (
        <PickerSection label={t(($) => $.pickers.assignee.agents_group)}>
          {filteredAgents.map((a) => {
            const decision = canAssignAgentToIssue(a, {
              userId,
              role:
                memberRole === "owner" ||
                memberRole === "admin" ||
                memberRole === "member"
                  ? memberRole
                  : null,
            });
            const actionState = agentActionState(a);
            const kind = agentExecutionKind(a);
            const allowed = decision.allowed && actionState === "ready";
            return (
              <PickerItem
                key={a.id}
                selected={isSelected("agent", a.id)}
                disabled={!allowed}
                tooltip={
                  !decision.allowed
                    ? decision.message
                    : actionState === "unsupported"
                      ? t(($) => $.pickers.assignee.agent_execution_unsupported)
                      : actionState === "needs-runtime"
                        ? t(($) => $.pickers.assignee.agent_runtime_required)
                        : undefined
                }
                onClick={() => {
                  if (!allowed) return;
                  chooseAssignee("agent", a.id, a);
                }}
              >
                <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
                <span className={`truncate ${allowed ? "" : "text-muted-foreground"}`}>{a.name}</span>
                {kind === "external" && (
                  <span className="min-w-0 truncate text-micro text-muted-foreground">
                    {a.external_session?.display_name ||
                      a.external_session?.session_id ||
                      t(($) => $.pickers.assignee.agent_external_session)}
                  </span>
                )}
                {a.visibility === "private" && (
                  <Lock className="ml-auto h-3 w-3 text-muted-foreground" />
                )}
              </PickerItem>
            );
          })}
        </PickerSection>
      )}

      {/* Squads — group ownership; assigning to a squad routes the issue to
          its leader agent on the backend. */}
      {filteredSquads.length > 0 && (
        <PickerSection label={t(($) => $.pickers.assignee.squads_group)}>
          {filteredSquads.map((s) => {
            const leader = liveAgentsById.get(s.leader_id);
            const actionState = leader ? agentActionState(leader) : "unsupported";
            const squadAllowed = !!leader && actionState === "ready";
            return (
              <PickerItem
                key={s.id}
                selected={isSelected("squad", s.id)}
                disabled={!squadAllowed}
                tooltip={
                  squadAllowed
                    ? undefined
                    : actionState === "unsupported"
                      ? t(($) => $.pickers.assignee.squad_execution_unsupported)
                      : t(($) => $.pickers.assignee.squad_runtime_required)
                }
                onClick={() => {
                  if (!squadAllowed) return;
                  chooseAssignee("squad", s.id, leader!);
                }}
              >
                <ActorAvatar actorType="squad" actorId={s.id} size="sm" />
                <span className="truncate">{s.name}</span>
              </PickerItem>
            );
          })}
        </PickerSection>
      )}

      {filteredMembers.length === 0 &&
        filteredAgents.length === 0 &&
        filteredSquads.length === 0 &&
        filter && <PickerEmpty />}
    </>
  );
}
