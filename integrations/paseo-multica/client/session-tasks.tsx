import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useCallback, useEffect, useRef, useState } from "react";
import { Pressable, Text, View } from "react-native";
import { taskExpansion, tasksSummary } from "../shared/contracts";
import { TaskCard } from "./task-card";
import { taskStatusLabel, type SessionTaskItem, type SessionTasksView } from "../shared/tasks";

type Colors = PluginAgentPanelProps["theme"]["colors"];

export function SessionTasksSection({ agentId, theme }: Pick<PluginAgentPanelProps, "agentId" | "theme">) {
  const getTasks = useRpc(tasksSummary);
  const expansion = useRpc(taskExpansion);
  const [expandedIds, setExpandedIds] = useState<string[]>([]);
  const [expansionReady, setExpansionReady] = useState(false);
  const [expansionError, setExpansionError] = useState("");
  const writes = useRef<Promise<unknown>>(Promise.resolve());
  const [view, setView] = useState<SessionTasksView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try { setView(await getTasks({ agentId })); }
    catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { setLoading(false); }
  }, [agentId, getTasks]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { let active = true; setExpansionReady(false); void expansion({ agentId }).then((ids) => { if (active) { setExpandedIds(ids); setExpansionReady(true); } }, (cause: unknown) => { if (active) { setExpansionError(String(cause)); setExpansionReady(true); } }); return () => { active = false; }; }, [agentId, expansion]);
  const toggle = useCallback((issueId: string, expanded: boolean) => {
    setExpandedIds((ids) => expanded ? [...ids.filter((id) => id !== issueId), issueId] : ids.filter((id) => id !== issueId));
    writes.current = writes.current.catch(() => {}).then(() => expansion({ agentId, issueId, expanded })).then(() => setExpansionError(""), (cause: unknown) => setExpansionError(String(cause)));
  }, [agentId, expansion]);
  const colors = theme.colors;
  return <View style={{ gap: 12 }}>
    {expansionError ? <Text style={{ color: colors.statusDanger }}>Could not save expanded tasks: {expansionError}</Text> : null}
    {loading || !expansionReady ? <Text style={{ color: colors.foregroundMuted }}>Loading tasks…</Text>
      : error ? <Text style={{ color: colors.statusDanger }}>{error}</Text>
      : view && view.state !== "ready" ? <Text style={{ color: colors.foregroundMuted }}>{view.reason}</Text>
      : view?.state === "ready" ? <>
        <TaskGroup title="Needs your attention" items={[
          ...view.accepting.map((item) => ({ item, detail: `Awaiting your acceptance${item.deliveryAt ? ` · ${formatDate(item.deliveryAt)}` : ""}` })),
          ...(view.requests ?? []).map((item) => ({ item, detail: `Executor mentioned you · awaiting your reply${item.requestAt ? ` · ${formatDate(item.requestAt)}` : ""}` })),
          ...view.owned.filter((item) => item.status === "blocked").map((item) => ({ item, detail: "Your task is blocked" })),
        ]} colors={colors} agentId={agentId} expandedIds={expandedIds} toggle={toggle} />
        <TaskGroup title="Delegated by you" items={(view.delegated ?? view.returning).map((item) => ({ item, detail: [item.executorName, delegationStatus(item), elapsed(item.activityAt ?? item.acceptedAt), item.blockedReason ? `From executor's latest comment: ${item.blockedReason}` : ""].filter(Boolean).join(" · ") }))} colors={colors} agentId={agentId} expandedIds={expandedIds} toggle={toggle} />
        <TaskGroup title="Your tasks" items={view.owned.map((item) => ({ item, detail: [taskStatusLabel(item), item.childCount ? `${item.closedChildCount ?? 0}/${item.childCount} children closed` : ""].filter(Boolean).join(" · ") }))} colors={colors} agentId={agentId} expandedIds={expandedIds} toggle={toggle} />
        {view.truncated ? <Text style={{ color: colors.foregroundMuted }}>Some tasks are not shown.</Text> : null}
      </> : null}
    <Pressable accessibilityRole="button" disabled={loading} onPress={() => void load()}><Text style={{ color: colors.accent }}>Refresh</Text></Pressable>
  </View>;
}

function delegationStatus(item: SessionTaskItem): string {
  if (item.status === "blocked") return "Blocked";
  if (item.pendingAcceptance) return "Awaiting acceptance";
  if (item.status === "in_review") return "In review";
  if (item.taskStatus === "running") return "Running";
  return taskStatusLabel(item);
}

function TaskGroup({ title, items, colors, agentId, expandedIds, toggle }: { title: string; items: { item: SessionTaskItem; detail: string }[]; colors: Colors; agentId: string; expandedIds: string[]; toggle(id: string, expanded: boolean): void }) {
  if (items.length === 0) return <Text style={{ color: colors.foregroundMuted }}>{title} · 0</Text>;
  return <View style={{ gap: 6 }}>
    <Text style={{ color: colors.foreground, fontWeight: "600" }}>{title} · {items.length}</Text>
    {items.map(({ item, detail }) =>
      <TaskCard key={`${item.issueId}:${item.role}:${item.requestId ?? ""}`} item={item} detail={detail} colors={colors} agentId={agentId} expandedIds={expandedIds} toggle={toggle} />)}
  </View>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
}

function elapsed(value?: string): string {
  if (!value) return "";
  const milliseconds = Date.now() - new Date(value).valueOf();
  if (!Number.isFinite(milliseconds)) return "";
  const minutes = Math.max(0, Math.floor(milliseconds / 60000));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return hours < 24 ? `${hours}h` : `${Math.floor(hours / 24)}d`;
}
