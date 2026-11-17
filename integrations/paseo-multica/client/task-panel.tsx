import type { PluginClientContext, PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useEffect, useId, useRef } from "react";
import { Pressable, Text, View } from "react-native";
import { MulticaSurface } from "./surface";

export const TASK_PANEL_ID = "task-detail";
const MAX_HISTORY = 10;
const ISSUE_PATH = /^\/[A-Za-z0-9][A-Za-z0-9_-]*\/issues\/[A-Za-z0-9][A-Za-z0-9_-]*$/;

type TaskState = { scope?: string; paths: string[] };
type TaskContext = Pick<PluginAgentPanelProps, "host" | "workspaceId" | "agentId">;
const taskScope = ({ host, workspaceId, agentId }: TaskContext) => JSON.stringify([host.id, workspaceId, agentId]);
export function parseTaskState(value: unknown): TaskState {
  if (!value || typeof value !== "object" || !Array.isArray((value as TaskState).paths)) return { paths: [] };
  const paths: unknown[] = (value as TaskState).paths;
  if (paths.length > MAX_HISTORY || paths.some((path) => typeof path !== "string" || !ISSUE_PATH.test(path))) return { paths: [] };
  return { ...(typeof (value as TaskState).scope === "string" ? { scope: (value as TaskState).scope } : {}), paths: paths as string[] };
}
export function issuePath(url: string): string | null {
  try {
    const parsed = new URL(url);
    if (!/^https?:$/.test(parsed.protocol) || parsed.search || parsed.hash || !ISSUE_PATH.test(parsed.pathname)) return null;
    return parsed.pathname;
  } catch { return null; }
}
export function visitTask(state: TaskState, path: string): TaskState {
  if (!ISSUE_PATH.test(path) || state.paths.at(-1) === path) return state;
  return { ...state, paths: [...state.paths, path].slice(-MAX_HISTORY) };
}

export function createTaskPanels(client: PluginClientContext) {
  const pending = new Map<string, string[]>();
  const latest = new Map<string, string>();
  const listeners = new Map<string, Set<(path: string) => void>>();
  const Component = (props: PluginAgentPanelProps) => {
    const instanceId = useId();
    const scope = taskScope(props);
    const stored = parseTaskState(props.tab.state);
    const state: TaskState = stored.scope === scope ? stored : { scope, paths: [] };
    const current = useRef(state);
    current.current = state;
    const setState = props.tab.setState;
    useEffect(() => {
      const receive = (path: string) => {
        const next = visitTask(current.current, path);
        if (next === current.current) return;
        current.current = next;
        setState(next);
      };
      const group = listeners.get(scope) ?? new Set();
      group.add(receive);
      listeners.set(scope, group);
      const queued = pending.get(scope) ?? [];
      pending.delete(scope);
      for (const path of queued) receive(path);
      const selected = latest.get(scope);
      if (selected && queued.length === 0) receive(selected);
      return () => { group.delete(receive); if (group.size === 0) listeners.delete(scope); };
    }, [scope, setState]);
    const path = state.paths.at(-1);
    return <View style={{ flex: 1, backgroundColor: props.theme.colors.surface0 }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: 12, padding: 10 }}>
        {state.paths.length > 1 ? <Pressable accessibilityRole="button" onPress={() => { const next = { ...current.current, paths: current.current.paths.slice(0, -1) }; current.current = next; latest.set(scope, next.paths.at(-1)!); setState(next); }}><Text style={{ color: props.theme.colors.accent }}>Back</Text></Pressable> : null}
        <Text style={{ color: props.theme.colors.foreground, fontWeight: "600" }}>{path?.split("/").at(-1) ?? "Task details"}</Text>
      </View>
      {path ? <MulticaSurface key={path} {...props} path={path} pageKey={`${props.agentId}:${instanceId}`} /> : <Text style={{ color: props.theme.colors.foregroundMuted, padding: 10 }}>Select a task to open its details.</Text>}
    </View>;
  };
  const remove = client.addWorkspacePanel({ id: TASK_PANEL_ID, title: "Task details", icon: "ListTodo", context: "agent", locations: ["explorer"], Component });
  return {
    open(context: TaskContext, url: string) {
      const { workspaceId, agentId } = context;
      const scope = taskScope(context);
      const path = issuePath(url);
      if (!path) return;
      client.openPanel(TASK_PANEL_ID, { workspaceId, agentId, location: "explorer" });
      latest.set(scope, path);
      const group = listeners.get(scope);
      if (group?.size) for (const receive of group) receive(path);
      else pending.set(scope, [...(pending.get(scope) ?? []), path].slice(-MAX_HISTORY));
    },
    dispose() { remove(); listeners.clear(); pending.clear(); latest.clear(); },
  };
}
