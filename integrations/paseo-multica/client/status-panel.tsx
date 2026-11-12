import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useCallback, useEffect, useState } from "react";
import { Pressable, ScrollView, Text, View } from "react-native";
import { statusSession, type StatusView } from "../shared/contracts";
import { SessionTasksSection } from "./session-tasks";

const STATE_LABEL: Record<StatusView["state"], string> = {
  connected: "Connected", invalid: "Needs attention", unverified: "Not verified", disconnected: "Not connected",
};

export function StatusPanel({ agentId, theme }: PluginAgentPanelProps) {
  const getStatus = useRpc(statusSession);
  const [view, setView] = useState<StatusView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState(false);
  const [loading, setLoading] = useState(true);
  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try { setView(await getStatus({ agentId })); }
    catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { setLoading(false); }
  }, [agentId, getStatus]);
  useEffect(() => { void load(); }, [load]);
  const colors = theme.colors;
  return <ScrollView style={{ backgroundColor: colors.surface0 }} contentContainerStyle={{ gap: 16, padding: 12 }}>
    <SessionTasksSection agentId={agentId} theme={theme} />
    <View style={{ gap: 6 }}>
      <Pressable accessibilityRole="button" accessibilityState={{ expanded }} onPress={() => setExpanded((value) => !value)}>
        <Text style={{ color: colors.foregroundMuted }}>{loading ? "Checking connection…" : view ? STATE_LABEL[view.state] : "Connection unavailable"}{view?.expiresAt ? ` · Credential expires ${view.expiresAt.slice(0, 7)}` : ""} {expanded ? "▴" : "▾"}</Text>
      </Pressable>
      {error ?? view?.reason ? <Text style={{ color: colors.statusDanger }}>{error ?? view?.reason}</Text> : null}
      {expanded ? <View style={{ gap: 4 }}>
        {[`External agent: ${view?.agentId ?? "—"}`, `Workspace: ${view?.workspaceId ?? "—"}`, `Return address: ${view?.returnAddress ?? "—"}`, `Delivery daemon: ${view?.daemonId ?? "—"}`, `Credential expires: ${view?.expiresAt ?? "—"}`].map((line) => <Text key={line} selectable style={{ color: colors.foregroundMuted }}>{line}</Text>)}
        <Pressable accessibilityRole="button" disabled={loading} onPress={() => void load()}><Text style={{ color: colors.accent }}>Refresh connection</Text></Pressable>
      </View> : null}
    </View>
  </ScrollView>;
}
