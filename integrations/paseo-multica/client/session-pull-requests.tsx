import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useState } from "react";
import { Linking, Pressable, Text, View } from "react-native";
import { sessionPullRequests } from "../shared/contracts";
import { needsAttention, type PullRequest } from "../shared/pull-requests";

export function SessionPullRequests({ agentId, theme, openTask }: Pick<PluginAgentPanelProps, "agentId" | "theme"> & { openTask(url: string, identifier?: string): void }) {
  const fetch = useRpc(sessionPullRequests);
  const [open, setOpen] = useState(false);
  const [items, setItems] = useState<PullRequest[] | null>(null);
  const [error, setError] = useState("");
  const colors = theme.colors;
  async function load() {
    try { const result = await fetch({ agentId }); setItems(result.pull_requests.sort((a, b) => Number(needsAttention(b)) - Number(needsAttention(a)))); setError(""); }
    catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
  }
  return <View style={{ gap: 6 }}>
    <Pressable accessibilityRole="button" accessibilityState={{ expanded: open }} onPress={() => { setOpen(!open); if (!open) void load(); }}><Text style={{ color: colors.foreground, fontWeight: "600" }}>Related pull requests {open ? "▴" : "▾"}</Text></Pressable>
    {open && error ? <Text style={{ color: colors.statusDanger }}>{error}</Text> : null}
    {open && !error && !items ? <Text style={{ color: colors.foregroundMuted }}>Loading…</Text> : null}
    {open && items?.length === 0 ? <Text style={{ color: colors.foregroundMuted }}>No related pull requests.</Text> : null}
    {open && items?.map((pr) => <View key={pr.id} style={{ gap: 3 }}>
      <Pressable accessibilityRole="link" onPress={() => void Linking.openURL(pr.html_url)}><Text style={{ color: colors.accent }}>{pr.repo_owner}/{pr.repo_name}#{pr.number} · {pr.title}</Text></Pressable>
      <Text style={{ color: colors.foregroundMuted }}>{pr.state} · CI: {pr.checks_conclusion ?? pr.checks_rollup ?? "unknown"} · review: {pr.review?.state ?? "unknown"} · conflicts: {pr.mergeable ?? "unknown"}</Text>
      {pr.source_url && /^https?:\/\//i.test(pr.source_url) ? <Pressable accessibilityRole="link" onPress={() => openTask(pr.source_url!, pr.source_identifier)}><Text style={{ color: colors.accent }}>{pr.source_identifier}</Text></Pressable> : <Text style={{ color: colors.foregroundMuted }}>{pr.source_identifier}</Text>}
    </View>)}
    {open ? <Pressable accessibilityRole="button" onPress={() => void load()}><Text style={{ color: colors.accent }}>Refresh</Text></Pressable> : null}
  </View>;
}
