import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useCallback, useEffect, useState } from "react";
import { Linking, Pressable, Text, View } from "react-native";
import { tasksSummary } from "../shared/contracts";
import { taskStatusLabel, type SessionTaskItem, type SessionTasksView } from "../shared/tasks";

type Colors = PluginAgentPanelProps["theme"]["colors"];

export function SessionTasksSection({ agentId, theme }: Pick<PluginAgentPanelProps, "agentId" | "theme">) {
  const getTasks = useRpc(tasksSummary);
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
  const colors = theme.colors;
  return <View style={{ gap: 12 }}>
    {loading ? <Text style={{ color: colors.foregroundMuted }}>Loading tasks…</Text>
      : error ? <Text style={{ color: colors.statusDanger }}>{error}</Text>
      : view && view.state !== "ready" ? <Text style={{ color: colors.foregroundMuted }}>{view.reason}</Text>
      : view?.state === "ready" ? <>
        <TaskGroup title="Needs your attention" items={[
          ...view.accepting.map((item) => ({ item, detail: `Awaiting your acceptance${item.deliveryAt ? ` · ${formatDate(item.deliveryAt)}` : ""}` })),
          ...view.owned.filter((item) => item.status === "blocked").map((item) => ({ item, detail: "Your task is blocked" })),
        ]} colors={colors} />
        <TaskGroup title="Delegated by you" items={(view.delegated ?? view.returning).map((item) => ({ item, detail: [item.executorName, delegationStatus(item), elapsed(item.activityAt ?? item.acceptedAt)].filter(Boolean).join(" · ") }))} colors={colors} />
        <TaskGroup title="Your tasks" items={view.owned.map((item) => ({ item, detail: [taskStatusLabel(item), item.childCount ? `${item.closedChildCount ?? 0}/${item.childCount} children closed` : ""].filter(Boolean).join(" · ") }))} colors={colors} />
        {view.truncated ? <Text style={{ color: colors.foregroundMuted }}>Some tasks are not shown.</Text> : null}
      </> : null}
    <Pressable accessibilityRole="button" disabled={loading} onPress={() => void load()}><Text style={{ color: colors.accent }}>Refresh</Text></Pressable>
  </View>;
}

function delegationStatus(item: SessionTaskItem): string {
  if (item.pendingAcceptance) return "Awaiting acceptance";
  if (item.status === "blocked") return "Blocked";
  if (item.status === "in_review") return "In review";
  if (item.taskStatus === "running") return "Running";
  return taskStatusLabel(item);
}

function TaskGroup({ title, items, colors }: { title: string; items: { item: SessionTaskItem; detail: string }[]; colors: Colors }) {
  return <View style={{ gap: 6 }}>
    <Text style={{ color: colors.foreground, fontWeight: "600" }}>{title} · {items.length}</Text>
    {items.length === 0 ? <Text style={{ color: colors.foregroundMuted }}>None</Text> : items.map(({ item, detail }) =>
      <Pressable key={`${item.issueId}:${item.role}`} accessibilityRole={item.url ? "link" : undefined} disabled={!item.url} onPress={() => { if (item.url) void Linking.openURL(item.url); }} style={{ backgroundColor: colors.surface1, borderColor: colors.border, borderWidth: 1, borderRadius: 8, padding: 10, gap: 4 }}>
        <Text style={{ color: colors.foreground, fontWeight: "600" }}>{item.identifier ?? item.issueId} · {item.title}</Text>
        <Text style={{ color: colors.foregroundMuted }}>{detail}</Text>
      </Pressable>)}
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
