import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { copyText } from "@getpaseo/plugin/client/ui";
import { useEffect, useState } from "react";
import { Linking, Pressable, Text, View } from "react-native";
import { taskDetail } from "../shared/contracts";
import { detailWireSchemas, type DetailSection } from "../shared/task-details";
import { taskStatusLabel, type SessionTaskItem } from "../shared/tasks";

type Colors = PluginAgentPanelProps["theme"]["colors"];
type Props = { agentId: string; item: SessionTaskItem; detail: string; colors: Colors; expandedIds: string[]; toggle(id: string, expanded: boolean): void };

export function TaskCard(props: Props) {
  const { item, detail, colors, expandedIds, toggle } = props;
  const expanded = expandedIds.includes(item.issueId);
  const [copyError, setCopyError] = useState("");
  return <View style={{ backgroundColor: colors.surface1, borderColor: colors.border, borderWidth: 1, borderRadius: 8, padding: 10, gap: 4 }}>
    <Pressable accessibilityRole="button" accessibilityState={{ expanded }} onPress={() => toggle(item.issueId, !expanded)}>
      <Text style={{ color: colors.foreground, fontWeight: "600" }}>{expanded ? "▾" : "▸"} {item.identifier ?? item.issueId} · {item.title}</Text>
      <Text style={{ color: colors.foregroundMuted }}>{detail}</Text>
    </Pressable>
    {expanded ? <>
      <View style={{ flexDirection: "row", gap: 16 }}>
        {item.url ? <Pressable accessibilityRole="link" onPress={() => { void Linking.openURL(item.url!); }}><Text style={{ color: colors.accent }}>Open task</Text></Pressable> : null}
        <Pressable accessibilityRole="button" onPress={() => { void copyText(item.identifier ?? item.issueId).then(() => setCopyError(""), () => setCopyError("Could not copy identifier.")); }}><Text style={{ color: colors.accent }}>Copy ID</Text></Pressable>
      </View>
      {copyError ? <Text style={{ color: colors.statusDanger }}>{copyError}</Text> : null}
      {(["contract", "runs", "comments", "prs", "children"] as const).map((section) => <Detail key={section} {...props} section={section} />)}
    </> : null}
  </View>;
}

function Detail(props: Props & { section: DetailSection }) {
  const { agentId, item, colors, section } = props;
  const load = useRpc(taskDetail);
  const [data, setData] = useState<unknown>();
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let active = true;
    setData(undefined); setError("");
    void load({ agentId, issueId: item.issueId, section }).then((result) => {
      if (!active) return;
      if (!result.ok) { setError(result.error); return; }
      const parsed = detailWireSchemas[section].safeParse(result.data);
      if (parsed.success) setData(parsed.data); else setError(`Invalid ${section} response.`);
    }, (cause: unknown) => { if (active) setError(cause instanceof Error ? cause.message : String(cause)); });
    return () => { active = false; };
  }, [agentId, item.issueId, section, load, attempt]);
  if (error) return <View><Text style={{ color: colors.statusDanger }}>{section}: {error}</Text><Pressable accessibilityRole="button" onPress={() => setAttempt((n) => n + 1)}><Text style={{ color: colors.accent }}>Retry {section}</Text></Pressable></View>;
  if (data === undefined) return <Text style={{ color: colors.foregroundMuted }}>Loading {section}…</Text>;
  const textStyle = { color: colors.foregroundMuted };
  if (section === "contract") {
    const parsed = detailWireSchemas.contract.parse(data);
    const version = parsed.contract?.versions.at(-1);
    if (!version) return <Text style={textStyle}>No contract</Text>;
    const pending = parsed.pending_actor;
    const role = pending && parsed.acceptor?.id === pending.id && parsed.acceptor.type === pending.type ? "acceptor" : pending && parsed.executor?.id === pending.id && parsed.executor.type === pending.type ? "executor" : pending?.type;
    const delivery = parsed.contract?.deliveries.filter((d) => d.version === version.number).at(-1);
    return <Text style={textStyle}>Contract v{version.number} · {parsed.accepted.length}/{version.criteria.length} accepted{parsed.stale ? " · Contract stale" : role ? ` · Pending ${role}` : ""}{!parsed.stale && role === "acceptor" && delivery ? ` · Delivered ${formatDate(delivery.created_at)}` : ""}</Text>;
  }
  if (section === "runs") {
    const runs = detailWireSchemas.runs.parse(data);
    const run = runs.find((r) => r.status === "running") ?? runs[0];
    if (!run) return <Text style={textStyle}>No runs</Text>;
    const start = run.started_at ?? run.created_at;
    return <Text style={textStyle}>Run · {run.agent_name ?? run.agent_id} · {run.status} · {formatDate(start)} · {duration(start, run.completed_at)}</Text>;
  }
  if (section === "comments") return <View>{detailWireSchemas.comments.parse(data).slice(-2).reverse().map((comment, i) => <Text key={i} style={textStyle}>{comment.author_name ?? comment.author_id} · {duration(comment.created_at)} ago · {comment.content.split("\n")[0]}</Text>)}</View>;
  if (section === "prs") return <View>{detailWireSchemas.prs.parse(data).pull_requests.map((pr) => <Pressable key={pr.html_url} accessibilityRole="link" onPress={() => { if (/^https?:\/\//i.test(pr.html_url)) void Linking.openURL(pr.html_url); }}><Text style={{ color: colors.accent }}>PR #{pr.number} · {pr.title}</Text></Pressable>)}</View>;
  const children = detailWireSchemas.children.parse(data).issues;
  return <View style={{ gap: 6 }}>{children.map((child) => {
    const url = item.url ? item.url.replace(/[^/]+\/?$/, encodeURIComponent(child.identifier)) : undefined;
    const childItem: SessionTaskItem = { issueId: child.id, identifier: child.identifier, title: child.title, status: child.status, statusName: child.status_name || undefined, statusCategory: child.status_category ?? "", role: "executor", url, executorName: child.assignee_name ?? child.assignee_id ?? undefined };
    return <TaskCard key={child.id} {...props} item={childItem} detail={[taskStatusLabel(childItem), childItem.executorName].filter(Boolean).join(" · ")} />;
  })}</View>;
}

function formatDate(value: string): string { const date = new Date(value); return Number.isNaN(date.valueOf()) ? value : date.toLocaleString(); }
function duration(start: string, end?: string | null): string {
  const ms = (end ? Date.parse(end) : Date.now()) - Date.parse(start);
  if (!Number.isFinite(ms)) return "—";
  const minutes = Math.max(0, Math.floor(ms / 60000));
  return minutes < 60 ? `${minutes}m` : minutes < 1440 ? `${Math.floor(minutes / 60)}h` : `${Math.floor(minutes / 1440)}d`;
}
