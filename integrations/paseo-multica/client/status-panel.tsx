import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useCallback, useEffect, useState } from "react";
import { Pressable, ScrollView, Text, View } from "react-native";
import { statusSession, type StatusView } from "../shared/contracts";
import { BoardSection } from "./board-panel";

// Connected and disconnected sessions are already explained by the board; only
// a binding the board cannot judge (revoked or unverifiable) gets a footer.
const ATTENTION_LABEL: Partial<Record<StatusView["state"], string>> = { invalid: "连接需要处理", unverified: "连接未验证" };

export function StatusPanel(props: PluginAgentPanelProps & { openTask(url: string, identifier?: string): void }) {
  return <StatusPanelContent key={JSON.stringify([props.host.id, props.workspaceId, props.agentId])} {...props} />;
}

function StatusPanelContent({ agentId, theme, host, navigation, openTask }: PluginAgentPanelProps & { openTask(url: string, identifier?: string): void }) {
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
  const label = error ? "连接状态不可用" : view ? ATTENTION_LABEL[view.state] : undefined;
  return <ScrollView style={{ backgroundColor: colors.surface0 }} contentContainerStyle={{ gap: 24, padding: 12 }}>
    <BoardSection agentId={agentId} theme={theme} host={host} navigation={navigation} openTask={openTask} />
    {label ? <View style={{ gap: 4 }}>
      <Pressable accessibilityRole="button" accessibilityState={{ expanded }} onPress={() => setExpanded((value) => !value)}>
        <Text style={{ color: colors.foregroundMuted, fontSize: 12, fontWeight: "500" }}>{label} {expanded ? "▴" : "▾"}</Text>
      </Pressable>
      <Text style={{ color: colors.statusDanger, fontSize: 12 }}>{error ?? view?.reason}</Text>
      {expanded ? <View style={{ gap: 2 }}>
        {[`External agent: ${view?.agentId ?? "—"}`, `Workspace: ${view?.workspaceId ?? "—"}`, `Return address: ${view?.returnAddress ?? "—"}`, `Delivery daemon: ${view?.daemonId ?? "—"}`, `Credential expires: ${view?.expiresAt ?? "—"}`].map((line) => <Text key={line} selectable style={{ color: colors.foregroundMuted, fontSize: 12 }}>{line}</Text>)}
        <Pressable accessibilityRole="button" disabled={loading} onPress={() => void load()}><Text style={{ color: colors.accent, fontSize: 12 }}>重新检查</Text></Pressable>
      </View> : null}
    </View> : null}
  </ScrollView>;
}
